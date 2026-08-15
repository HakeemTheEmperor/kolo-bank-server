package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertDevice records a login from identityID/fingerprint, creating the
// device row on first sight or bumping last_seen_at otherwise. isNew
// reports whether this is the first time this fingerprint has been seen
// for this identity — callers use that to gate step-up / audit a new
// device (docs/banking-backend-spec.md §3.2: "device registration and
// trust").
func UpsertDevice(ctx context.Context, pool *pgxpool.Pool, identityID, fingerprint string) (deviceID string, isNew bool, err error) {
	err = pool.QueryRow(ctx, `
		INSERT INTO devices (identity_id, fingerprint)
		VALUES ($1, $2)
		ON CONFLICT (identity_id, fingerprint) DO UPDATE SET last_seen_at = now()
		RETURNING id::text, (xmax = 0) AS inserted
	`, identityID, fingerprint).Scan(&deviceID, &isNew)
	if err != nil {
		return "", false, fmt.Errorf("auth: upsert device: %w", err)
	}
	return deviceID, isNew, nil
}

// TrustDevice marks a device as trusted, e.g. after the user confirms it
// out of band.
func TrustDevice(ctx context.Context, pool *pgxpool.Pool, deviceID string) error {
	tag, err := pool.Exec(ctx, `UPDATE devices SET trusted = true WHERE id = $1`, deviceID)
	if err != nil {
		return fmt.Errorf("auth: trust device: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("auth: trust device: device %q not found", deviceID)
	}
	return nil
}
