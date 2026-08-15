package externalpayments

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
)

type claimedRow struct {
	ID              string
	Direction       rails.Direction
	AccountID       string
	RailName        string
	CounterpartyRef string
	Amount          amountFields
	IdempotencyKey  string
	HoldID          *string
}

type amountFields struct {
	Minor    int64
	Currency string
}

const claimBatchSize = 20

// ProcessPending claims up to claimBatchSize pending transfers and runs
// each through its rail. FOR UPDATE SKIP LOCKED makes concurrent callers
// (or overlapping ticks) safe: each row is claimed by exactly one caller.
func (s *Service) ProcessPending(ctx context.Context) error {
	rows, err := s.claim(ctx, `
		WITH claimed AS (
			SELECT id FROM external_transfers WHERE status = 'pending' ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		UPDATE external_transfers SET status = 'processing', attempted_at = now()
		WHERE id IN (SELECT id FROM claimed)
		RETURNING id::text, direction, account_id::text, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id::text
	`, claimBatchSize)
	if err != nil {
		return fmt.Errorf("externalpayments: claim pending: %w", err)
	}

	for _, row := range rows {
		if err := s.finalize(ctx, row); err != nil {
			s.logger.ErrorContext(ctx, "externalpayments: finalize failed", slog.String("id", row.ID), slog.Any("error", err))
		}
	}
	return nil
}

// ResolveStuck is the in-flight resolver: rows still "processing" past
// stuckTimeout are retried to a definitive outcome, safe because both
// SimulatedRail's idempotency cache and the ledger's own idempotency layer
// key off values that don't change between attempts
// (docs/banking-backend-spec.md §4.4).
func (s *Service) ResolveStuck(ctx context.Context) error {
	rows, err := s.claim(ctx, `
		WITH stuck AS (
			SELECT id FROM external_transfers
			WHERE status = 'processing' AND attempted_at < now() - make_interval(secs => $2)
			ORDER BY attempted_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE external_transfers SET attempted_at = now()
		WHERE id IN (SELECT id FROM stuck)
		RETURNING id::text, direction, account_id::text, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id::text
	`, claimBatchSize, stuckTimeout.Seconds())
	if err != nil {
		return fmt.Errorf("externalpayments: claim stuck: %w", err)
	}

	for _, row := range rows {
		s.logger.WarnContext(ctx, "externalpayments: resolving stuck transfer", slog.String("id", row.ID))
		if err := s.finalize(ctx, row); err != nil {
			s.logger.ErrorContext(ctx, "externalpayments: resolve stuck failed", slog.String("id", row.ID), slog.Any("error", err))
		}
	}
	return nil
}

func (s *Service) claim(ctx context.Context, query string, args ...any) ([]claimedRow, error) {
	dbRows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer dbRows.Close()

	var out []claimedRow
	for dbRows.Next() {
		var row claimedRow
		if err := dbRows.Scan(&row.ID, &row.Direction, &row.AccountID, &row.RailName, &row.CounterpartyRef,
			&row.Amount.Minor, &row.Amount.Currency, &row.IdempotencyKey, &row.HoldID); err != nil {
			return nil, fmt.Errorf("scan claimed row: %w", err)
		}
		out = append(out, row)
	}
	return out, dbRows.Err()
}

// finalize calls the rail for row and records a definitive outcome. It is
// safe to call more than once for the same row — that's exactly what
// ResolveStuck relies on. A nil return doesn't mean success: an ambiguous
// rail error (including a caller-imposed timeout) is logged and the row is
// deliberately left "processing" for a later resolver pass.
func (s *Service) finalize(ctx context.Context, row claimedRow) error {
	rail, err := s.registry.Get(row.RailName)
	if err != nil {
		return err
	}

	result, err := rail.Send(ctx, rails.PaymentRequest{
		Direction:       row.Direction,
		CounterpartyRef: row.CounterpartyRef,
		AmountMinor:     row.Amount.Minor,
		Currency:        row.Amount.Currency,
		ClientReference: row.ID,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "externalpayments: rail call did not complete, leaving processing for resolver",
			slog.String("id", row.ID), slog.String("rail", row.RailName), slog.Any("error", err))
		return nil
	}

	if result.Status == rails.StatusFailed {
		if row.Direction == rails.DirectionOutbound && row.HoldID != nil {
			if err := s.ledgerSvc.ReleaseHold(ctx, *row.HoldID); err != nil {
				return fmt.Errorf("release hold on rail failure: %w", err)
			}
		}
		_, err := s.pool.Exec(ctx, `
			UPDATE external_transfers SET status = 'failed', rail_reference = $2, error = $3, completed_at = now() WHERE id = $1
		`, row.ID, result.RailReference, result.FailureReason)
		if err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		return nil
	}

	amount, err := ledger.NewMoney(row.Amount.Minor, row.Amount.Currency)
	if err != nil {
		return fmt.Errorf("externalpayments: rebuild amount: %w", err)
	}

	var ledgerTxnID string
	if row.Direction == rails.DirectionOutbound {
		if row.HoldID == nil {
			return fmt.Errorf("externalpayments: outbound transfer %s has no hold", row.ID)
		}
		txn, err := s.ledgerSvc.CaptureHold(ctx, *row.HoldID, row.IdempotencyKey)
		if err != nil {
			return fmt.Errorf("capture hold on rail success: %w", err)
		}
		ledgerTxnID = txn.ID
	} else {
		txn, err := s.ledgerSvc.Credit(ctx, row.AccountID, amount, row.IdempotencyKey)
		if err != nil {
			return fmt.Errorf("credit account on rail success: %w", err)
		}
		ledgerTxnID = txn.ID
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE external_transfers SET status = 'completed', rail_reference = $2, ledger_transaction_id = $3, completed_at = now() WHERE id = $1
	`, row.ID, result.RailReference, ledgerTxnID)
	if err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	return nil
}
