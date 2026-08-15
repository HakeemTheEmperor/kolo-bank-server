// Package webhooks implements signed webhook delivery with retry and
// backoff (docs/banking-backend-spec.md §3.6). An endpoint's secret is
// generated once, shown to the merchant, and sealed at rest via
// secrets.KeyProvider (internal/secrets) — the same seam
// internal/auth uses for TOTP secrets (internal/auth/mfa.go).
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
)

const secretKeyName = "webhook-endpoint-secret"

var ErrNotFound = errors.New("webhooks: endpoint not found")

type Endpoint struct {
	ID         string
	MerchantID string
	Mode       apikeys.Mode
	URL        string
	Active     bool
	CreatedAt  time.Time
}

type Service struct {
	pool *pgxpool.Pool
	kp   secrets.KeyProvider
}

func NewService(pool *pgxpool.Pool, kp secrets.KeyProvider) *Service {
	return &Service{pool: pool, kp: kp}
}

// CreateEndpoint registers a webhook URL, returning the signing secret
// once — it is never retrievable again, only its sealed form is stored.
func (s *Service) CreateEndpoint(ctx context.Context, merchantID string, mode apikeys.Mode, url string) (secret string, ep Endpoint, err error) {
	secret, err = generateSecret()
	if err != nil {
		return "", Endpoint{}, err
	}

	sealed, err := s.kp.Encrypt(ctx, secretKeyName, []byte(secret))
	if err != nil {
		return "", Endpoint{}, fmt.Errorf("webhooks: seal secret: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (merchant_id, mode, url, secret_encrypted)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, merchant_id::text, mode, url, active, created_at
	`, merchantID, mode, url, sealed).Scan(&ep.ID, &ep.MerchantID, &ep.Mode, &ep.URL, &ep.Active, &ep.CreatedAt)
	if err != nil {
		return "", Endpoint{}, fmt.Errorf("webhooks: create endpoint: %w", err)
	}
	return secret, ep, nil
}

func (s *Service) ListEndpoints(ctx context.Context, merchantID string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, merchant_id::text, mode, url, active, created_at
		FROM webhook_endpoints WHERE merchant_id = $1 ORDER BY created_at DESC
	`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list endpoints: %w", err)
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var ep Endpoint
		if err := rows.Scan(&ep.ID, &ep.MerchantID, &ep.Mode, &ep.URL, &ep.Active, &ep.CreatedAt); err != nil {
			return nil, fmt.Errorf("webhooks: scan endpoint: %w", err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

func (s *Service) DeactivateEndpoint(ctx context.Context, endpointID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET active = false WHERE id = $1 AND active`, endpointID)
	if err != nil {
		return fmt.Errorf("webhooks: deactivate endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) activeEndpoints(ctx context.Context, merchantID string, mode apikeys.Mode) ([]endpointWithSecret, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, url, secret_encrypted FROM webhook_endpoints
		WHERE merchant_id = $1 AND mode = $2 AND active
	`, merchantID, mode)
	if err != nil {
		return nil, fmt.Errorf("webhooks: find active endpoints: %w", err)
	}
	defer rows.Close()

	var out []endpointWithSecret
	for rows.Next() {
		var e endpointWithSecret
		if err := rows.Scan(&e.ID, &e.URL, &e.sealedSecret); err != nil {
			return nil, fmt.Errorf("webhooks: scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) unseal(ctx context.Context, sealed []byte) (string, error) {
	plain, err := s.kp.Decrypt(ctx, secretKeyName, sealed)
	if err != nil {
		return "", fmt.Errorf("webhooks: unseal secret: %w", err)
	}
	return string(plain), nil
}

type endpointWithSecret struct {
	ID           string
	URL          string
	sealedSecret []byte
}

func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("webhooks: generate secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(b), nil
}
