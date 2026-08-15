package risk

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotHeld is returned when resolving a transfer that isn't currently held.
var ErrNotHeld = errors.New("risk: transfer is not held")

// Approve clears a held transfer's risk_status back to 'clear', the
// case-management action an investigator takes to let ProcessPending pick
// it back up on its next tick — the manual half of "held ... in real time"
// (docs/banking-backend-spec.md §7 Phase 7 exit criteria).
func (s *Service) Approve(ctx context.Context, transferID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE external_transfers SET risk_status = 'clear' WHERE id = $1 AND risk_status = 'held'
	`, transferID)
	if err != nil {
		return fmt.Errorf("risk: approve: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotHeld
	}
	return nil
}

// Block escalates a held transfer straight to blocked: fails it and
// releases its hold, the same as an automatic block reached by Assess.
func (s *Service) Block(ctx context.Context, transferID string) error {
	var holdID *string
	err := s.pool.QueryRow(ctx, `
		SELECT hold_id::text FROM external_transfers WHERE id = $1 AND risk_status = 'held'
	`, transferID).Scan(&holdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotHeld
		}
		return fmt.Errorf("risk: load held transfer: %w", err)
	}
	return s.failAndRelease(ctx, transferID, holdID, "blocked by investigator")
}
