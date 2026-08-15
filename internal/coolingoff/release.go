package coolingoff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

type claimedRow struct {
	ID             string
	FromAccountID  string
	ToAccountID    string
	AmountMinor    int64
	Currency       string
	HoldID         string
	IdempotencyKey string
}

const claimBatchSize = 20

// ReleaseMatured captures every cooling-off hold past its release_at into
// a real transfer, unless the sender cancelled it first. It also retries
// any row left 'processing' by a crash on a previous tick before claiming
// new ones — ledger.Service.ReleaseHold and payments' underlying Transfer
// are both idempotent (ReleaseHold on an already-released hold returns
// ErrHoldNotActive rather than corrupting state, Transfer keys off the
// row's own stored idempotency key), so simply retrying the same two
// steps is sufficient recovery; unlike internal/externalpayments there's
// no external rail call in the middle, so there's no ambiguous-timeout
// window to wait out with a separate stuck-resolver pass.
func (s *Service) ReleaseMatured(ctx context.Context) error {
	stuck, err := s.claim(ctx, `
		SELECT id::text, from_account_id::text, to_account_id::text, amount_minor, currency, hold_id::text, idempotency_key
		FROM cooling_off_transfers WHERE status = 'processing' ORDER BY release_at LIMIT $1 FOR UPDATE SKIP LOCKED
	`, claimBatchSize)
	if err != nil {
		return fmt.Errorf("coolingoff: claim stuck: %w", err)
	}

	due, err := s.claim(ctx, `
		WITH claimed AS (
			SELECT id FROM cooling_off_transfers
			WHERE status = 'pending' AND release_at <= now()
			ORDER BY release_at LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		UPDATE cooling_off_transfers SET status = 'processing'
		WHERE id IN (SELECT id FROM claimed)
		RETURNING id::text, from_account_id::text, to_account_id::text, amount_minor, currency, hold_id::text, idempotency_key
	`, claimBatchSize)
	if err != nil {
		return fmt.Errorf("coolingoff: claim due: %w", err)
	}

	for _, row := range append(stuck, due...) {
		if err := s.finalizeRelease(ctx, row); err != nil {
			s.logger.ErrorContext(ctx, "coolingoff: finalize release failed", slog.String("id", row.ID), slog.Any("error", err))
		}
	}
	return nil
}

func (s *Service) finalizeRelease(ctx context.Context, row claimedRow) error {
	if err := s.ledgerSvc.ReleaseHold(ctx, row.HoldID); err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
		return fmt.Errorf("release hold: %w", err)
	}

	amount, err := ledger.NewMoney(row.AmountMinor, row.Currency)
	if err != nil {
		return fmt.Errorf("rebuild amount: %w", err)
	}

	txn, err := s.ledgerSvc.Transfer(ctx, row.FromAccountID, row.ToAccountID, amount, row.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("transfer: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE cooling_off_transfers SET status = 'completed', completed_transaction_id = $2 WHERE id = $1
	`, row.ID, txn.ID); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	return nil
}

func (s *Service) claim(ctx context.Context, query string, args ...any) ([]claimedRow, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []claimedRow
	for rows.Next() {
		var r claimedRow
		if err := rows.Scan(&r.ID, &r.FromAccountID, &r.ToAccountID, &r.AmountMinor, &r.Currency, &r.HoldID, &r.IdempotencyKey); err != nil {
			return nil, fmt.Errorf("scan claimed row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
