// Package consent implements the connected-apps & consent dashboard
// (docs/banking-backend-spec.md §5.3): every third-party (merchant)
// authorization a customer has granted, viewable and one-tap revocable.
// A standalone OAuth-style grant — deliberately not layered onto
// payment_instrument_tokens or api_keys, neither of which link to a
// specific customer identity today.
package consent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

// ErrNotFound is returned when revoking or touching an authorization that
// doesn't exist, or doesn't belong to the calling identity.
var ErrNotFound = errors.New("consent: authorization not found")

type Authorization struct {
	ID         string
	IdentityID string
	MerchantID string
	Scopes     []string
	Status     Status
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Grant records identityID's authorization for merchantID with scopes.
// Idempotent per (identity, merchant): re-granting an active pair updates
// its scopes, and re-granting a previously revoked pair reactivates it
// rather than creating a duplicate row.
func (s *Service) Grant(ctx context.Context, identityID, merchantID string, scopes []string) (Authorization, error) {
	var a Authorization
	err := s.pool.QueryRow(ctx, `
		INSERT INTO customer_authorizations (identity_id, merchant_id, scopes)
		VALUES ($1, $2, $3)
		ON CONFLICT (identity_id, merchant_id) DO UPDATE
		SET scopes = EXCLUDED.scopes, status = 'active', revoked_at = NULL
		RETURNING id::text, identity_id::text, merchant_id::text, scopes, status, created_at, last_used_at, revoked_at
	`, identityID, merchantID, scopes).Scan(
		&a.ID, &a.IdentityID, &a.MerchantID, &a.Scopes, &a.Status, &a.CreatedAt, &a.LastUsedAt, &a.RevokedAt,
	)
	if err != nil {
		return Authorization{}, fmt.Errorf("consent: grant: %w", err)
	}
	return a, nil
}

// ListByIdentity returns identityID's authorizations, newest first.
func (s *Service) ListByIdentity(ctx context.Context, identityID string) ([]Authorization, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, identity_id::text, merchant_id::text, scopes, status, created_at, last_used_at, revoked_at
		FROM customer_authorizations WHERE identity_id = $1
		ORDER BY created_at DESC
	`, identityID)
	if err != nil {
		return nil, fmt.Errorf("consent: list: %w", err)
	}
	defer rows.Close()

	var out []Authorization
	for rows.Next() {
		var a Authorization
		if err := rows.Scan(&a.ID, &a.IdentityID, &a.MerchantID, &a.Scopes, &a.Status, &a.CreatedAt, &a.LastUsedAt, &a.RevokedAt); err != nil {
			return nil, fmt.Errorf("consent: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Revoke deactivates authorizationID, ownership-checked against
// identityID so one customer can't revoke another's grant.
func (s *Service) Revoke(ctx context.Context, identityID, authorizationID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE customer_authorizations SET status = 'revoked', revoked_at = now()
		WHERE id = $1 AND identity_id = $2 AND status = 'active'
	`, authorizationID, identityID)
	if err != nil {
		return fmt.Errorf("consent: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Touch records that authorizationID was actually used, for the
// dashboard's "last used" display.
func (s *Service) Touch(ctx context.Context, authorizationID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE customer_authorizations SET last_used_at = now() WHERE id = $1 AND status = 'active'`, authorizationID)
	if err != nil {
		return fmt.Errorf("consent: touch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
