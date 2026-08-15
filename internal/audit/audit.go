// Package audit writes the append-only audit_log trail required for every
// auth and onboarding event (docs/banking-backend-spec.md §3.2, §3.8).
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record writes an audit_log row inside the caller's transaction, so it
// commits atomically with whatever it's describing. actorID is nil for
// system-initiated events.
func Record(ctx context.Context, tx pgx.Tx, actorID *string, action, targetType, targetID string, metadata map[string]any) error {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("audit: marshal metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (actor_identity_id, action, target_type, target_id, metadata)
		VALUES ($1, $2, $3, $4, $5)
	`, actorID, action, targetType, targetID, metaJSON); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}

	return nil
}

// RecordNow writes an audit_log row in its own transaction, for events with
// no enclosing transaction to ride along with (e.g. a failed login attempt,
// where there is otherwise nothing to commit).
func RecordNow(ctx context.Context, pool *pgxpool.Pool, actorID *string, action, targetType, targetID string, metadata map[string]any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("audit: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := Record(ctx, tx, actorID, action, targetType, targetID, metadata); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
