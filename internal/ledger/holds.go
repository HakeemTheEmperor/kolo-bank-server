package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/idempotency"
)

// insertPostedTransaction creates a ledger_transactions row directly in the
// "posted" state and writes its balanced entries. Internal moves post
// synchronously; Phase 4's external rails and in-flight resolver are what
// need the "pending" state.
func insertPostedTransaction(ctx context.Context, tx pgx.Tx, txType TransactionType, idempotencyKey string, legs []entryLeg) (string, error) {
	var txnID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (idempotency_key, type, state)
		VALUES ($1, $2, 'posted')
		RETURNING id::text
	`, idempotencyKey, txType).Scan(&txnID); err != nil {
		return "", fmt.Errorf("ledger: create transaction: %w", err)
	}

	for _, leg := range legs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, amount_minor, currency)
			VALUES ($1, $2, $3, $4)
		`, txnID, leg.AccountID, leg.Amount.Minor, leg.Amount.Currency); err != nil {
			return "", translatePostingError(err)
		}
	}

	return txnID, nil
}

// PlaceHold reserves amount against accountID's available balance without
// posting a ledger entry. It is rejected if the hold would exceed the
// account's current available balance.
func (s *Service) PlaceHold(ctx context.Context, accountID string, amount Money) (Hold, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Hold{}, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var overdraft int64
	if err := tx.QueryRow(ctx, `SELECT overdraft_limit_minor FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&overdraft); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Hold{}, ErrAccountNotFound
		}
		return Hold{}, fmt.Errorf("ledger: lock account: %w", err)
	}

	var ledgerMinor, heldMinor int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries WHERE account_id = $1`, accountID).Scan(&ledgerMinor); err != nil {
		return Hold{}, fmt.Errorf("ledger: sum ledger entries: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM holds WHERE account_id = $1 AND status = 'active'`, accountID).Scan(&heldMinor); err != nil {
		return Hold{}, fmt.Errorf("ledger: sum active holds: %w", err)
	}

	available := ledgerMinor - heldMinor
	if available-amount.Minor < -overdraft {
		return Hold{}, ErrInsufficientBalance
	}

	var h Hold
	if err := tx.QueryRow(ctx, `
		INSERT INTO holds (account_id, amount_minor, status)
		VALUES ($1, $2, 'active')
		RETURNING id::text, account_id::text, amount_minor, status, created_at, expires_at
	`, accountID, amount.Minor).Scan(&h.ID, &h.AccountID, &h.Amount.Minor, &h.Status, &h.CreatedAt, &h.ExpiresAt); err != nil {
		return Hold{}, fmt.Errorf("ledger: place hold: %w", err)
	}
	h.Amount.Currency = amount.Currency

	if err := tx.Commit(ctx); err != nil {
		return Hold{}, fmt.Errorf("ledger: commit: %w", err)
	}

	return h, nil
}

// ReleaseHold cancels an active hold without moving funds.
func (s *Service) ReleaseHold(ctx context.Context, holdID string) error {
	return s.resolveHold(ctx, holdID, HoldStatusReleased)
}

func (s *Service) resolveHold(ctx context.Context, holdID string, target HoldStatus) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM holds WHERE id = $1 FOR UPDATE`, holdID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHoldNotFound
		}
		return fmt.Errorf("ledger: lock hold: %w", err)
	}
	if status != string(HoldStatusActive) {
		return ErrHoldNotActive
	}

	if _, err := tx.Exec(ctx, `UPDATE holds SET status = $2 WHERE id = $1`, holdID, target); err != nil {
		return fmt.Errorf("ledger: update hold status: %w", err)
	}

	return tx.Commit(ctx)
}

// CaptureHold converts an active hold into an actual posted debit against
// the held account, offset against the system account for its currency.
func (s *Service) CaptureHold(ctx context.Context, holdID string, idempotencyKey string) (Transaction, error) {
	hash := hashHoldCapture(holdID)

	respBytes, err := idempotency.Execute(ctx, s.pool, idempotencyKey, hash, idempotencyTTL,
		func(ctx context.Context, tx pgx.Tx) ([]byte, error) {
			var accountID, status string
			var amountMinor int64
			if err := tx.QueryRow(ctx,
				`SELECT account_id::text, amount_minor, status FROM holds WHERE id = $1 FOR UPDATE`,
				holdID,
			).Scan(&accountID, &amountMinor, &status); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrHoldNotFound
				}
				return nil, fmt.Errorf("ledger: lock hold: %w", err)
			}
			if status != string(HoldStatusActive) {
				return nil, ErrHoldNotActive
			}

			var currency string
			if err := tx.QueryRow(ctx, `SELECT currency FROM accounts WHERE id = $1`, accountID).Scan(&currency); err != nil {
				return nil, fmt.Errorf("ledger: get account currency: %w", err)
			}

			amount := Money{Minor: amountMinor, Currency: currency}
			systemAccount, err := systemAccountFor(currency)
			if err != nil {
				return nil, err
			}

			legs := []entryLeg{
				{AccountID: accountID, Amount: amount.Negate()},
				{AccountID: systemAccount, Amount: amount},
			}
			txnID, err := insertPostedTransaction(ctx, tx, TransactionTypeDebit, idempotencyKey, legs)
			if err != nil {
				return nil, err
			}

			if _, err := tx.Exec(ctx, `UPDATE holds SET status = 'captured' WHERE id = $1`, holdID); err != nil {
				return nil, fmt.Errorf("ledger: update hold status: %w", err)
			}

			result := transactionResult{TransactionID: txnID, State: string(TransactionStatePosted)}
			return json.Marshal(result)
		},
	)
	if err != nil {
		return Transaction{}, err
	}

	var result transactionResult
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return Transaction{}, fmt.Errorf("ledger: decode transaction result: %w", err)
	}

	return Transaction{
		ID:             result.TransactionID,
		IdempotencyKey: idempotencyKey,
		Type:           TransactionTypeDebit,
		State:          TransactionState(result.State),
	}, nil
}

func hashHoldCapture(holdID string) string {
	return "capture:" + holdID
}
