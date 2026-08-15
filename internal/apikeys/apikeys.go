// Package apikeys implements API-key issuance, scoping, and rotation per
// merchant (docs/banking-backend-spec.md §3.6). A merchant is simply a
// business identity (identity.KindBusiness) with keys attached.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mode distinguishes sandbox/test activity from live activity. It's the
// authenticated key's own mode, not a client-supplied value — that's what
// makes it a real isolation boundary rather than just a label. Other
// integration-API packages (tokens, charges, payouts, checkout, webhooks)
// import this type rather than redefining it, since a resource's mode
// always comes from the key that created it.
type Mode string

const (
	ModeSandbox Mode = "sandbox"
	ModeLive    Mode = "live"
)

type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

var (
	// ErrInvalidKey is returned for an unknown, malformed, or revoked key.
	ErrInvalidKey = errors.New("apikeys: invalid or revoked API key")
	// ErrNotFound is returned when looking up a key by ID that doesn't exist.
	ErrNotFound = errors.New("apikeys: not found")
)

type ApiKey struct {
	ID         string
	MerchantID string
	Mode       Mode
	Name       string
	KeyPrefix  string
	Scopes     []string
	Status     Status
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Create issues a new key, returning the raw value once — only its hash is
// ever stored (same pattern as auth.Service session tokens,
// internal/auth/session.go).
func (s *Service) Create(ctx context.Context, merchantID string, mode Mode, name string, scopes []string) (rawKey string, key ApiKey, err error) {
	rawKey, prefix, hash, err := generateKey(mode)
	if err != nil {
		return "", ApiKey{}, err
	}
	if scopes == nil {
		// A nil slice binds as SQL NULL, not the column's DEFAULT '{}',
		// which would violate scopes' NOT NULL constraint.
		scopes = []string{}
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (merchant_id, mode, name, key_prefix, key_hash, scopes)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, merchant_id::text, mode, name, key_prefix, scopes, status, created_at, revoked_at
	`, merchantID, mode, name, prefix, hash, scopes).Scan(
		&key.ID, &key.MerchantID, &key.Mode, &key.Name, &key.KeyPrefix, &key.Scopes, &key.Status, &key.CreatedAt, &key.RevokedAt,
	)
	if err != nil {
		return "", ApiKey{}, fmt.Errorf("apikeys: create: %w", err)
	}
	return rawKey, key, nil
}

// Authenticate resolves a raw bearer key to its record, rejecting unknown
// or revoked keys uniformly (no distinction, to avoid leaking which).
func (s *Service) Authenticate(ctx context.Context, rawKey string) (ApiKey, error) {
	hash := hashKey(rawKey)

	var key ApiKey
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, name, key_prefix, scopes, status, created_at, revoked_at
		FROM api_keys WHERE key_hash = $1
	`, hash).Scan(
		&key.ID, &key.MerchantID, &key.Mode, &key.Name, &key.KeyPrefix, &key.Scopes, &key.Status, &key.CreatedAt, &key.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApiKey{}, ErrInvalidKey
		}
		return ApiKey{}, fmt.Errorf("apikeys: authenticate: %w", err)
	}
	if key.Status != StatusActive {
		return ApiKey{}, ErrInvalidKey
	}
	return key, nil
}

// Rotate issues a fresh key with the same merchant/mode/scopes and
// immediately revokes the old one.
func (s *Service) Rotate(ctx context.Context, oldKeyID string) (rawKey string, key ApiKey, err error) {
	old, err := s.Get(ctx, oldKeyID)
	if err != nil {
		return "", ApiKey{}, err
	}

	rawKey, newKey, err := s.Create(ctx, old.MerchantID, old.Mode, old.Name, old.Scopes)
	if err != nil {
		return "", ApiKey{}, err
	}

	if err := s.Revoke(ctx, oldKeyID); err != nil {
		return "", ApiKey{}, err
	}

	return rawKey, newKey, nil
}

func (s *Service) Revoke(ctx context.Context, keyID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET status = 'revoked', revoked_at = now() WHERE id = $1 AND status = 'active'
	`, keyID)
	if err != nil {
		return fmt.Errorf("apikeys: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) List(ctx context.Context, merchantID string) ([]ApiKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, merchant_id::text, mode, name, key_prefix, scopes, status, created_at, revoked_at
		FROM api_keys WHERE merchant_id = $1 ORDER BY created_at DESC
	`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("apikeys: list: %w", err)
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var key ApiKey
		if err := rows.Scan(&key.ID, &key.MerchantID, &key.Mode, &key.Name, &key.KeyPrefix, &key.Scopes, &key.Status, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, fmt.Errorf("apikeys: scan: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Get fetches a key by id, used by callers that need to enforce
// ownership before Rotate/Revoke.
func (s *Service) Get(ctx context.Context, keyID string) (ApiKey, error) {
	var key ApiKey
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, name, key_prefix, scopes, status, created_at, revoked_at
		FROM api_keys WHERE id = $1
	`, keyID).Scan(&key.ID, &key.MerchantID, &key.Mode, &key.Name, &key.KeyPrefix, &key.Scopes, &key.Status, &key.CreatedAt, &key.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApiKey{}, ErrNotFound
		}
		return ApiKey{}, fmt.Errorf("apikeys: get: %w", err)
	}
	return key, nil
}

// HasScope reports whether key is authorized for scope.
func (k ApiKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func generateKey(mode Mode) (rawKey, prefix, hash string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("apikeys: generate key: %w", err)
	}
	modeTag := "live"
	if mode == ModeSandbox {
		modeTag = "test"
	}
	rawKey = fmt.Sprintf("kolo_%s_%s", modeTag, hex.EncodeToString(b))
	prefix = rawKey[:min(len(rawKey), 12)]
	return rawKey, prefix, hashKey(rawKey), nil
}

func hashKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}
