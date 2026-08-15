package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/idempotency"
)

// ErrTransactionNotFound is returned when reversing an unknown transaction.
var ErrTransactionNotFound = errors.New("ledger: transaction not found")

// ErrNotReversible is returned when reversing a transaction that isn't
// currently posted (already reversed, still pending, or failed).
var ErrNotReversible = errors.New("ledger: transaction is not reversible")

// ReverseTransaction posts compensating entries for originalTransactionID
// (each original leg negated) as a new reversal-typed transaction, and
// transitions the original posted -> reversed
// (docs/banking-backend-spec.md §3.4: "reversals and refunds that post
// compensating ledger entries rather than editing history"). Atomic and
// idempotent, mirroring postTransaction (internal/ledger/service.go).
func (s *Service) ReverseTransaction(ctx context.Context, originalTransactionID, idempotencyKey string) (Transaction, error) {
	hash := hashReversal(originalTransactionID)

	respBytes, err := idempotency.Execute(ctx, s.pool, idempotencyKey, hash, idempotencyTTL,
		func(ctx context.Context, tx pgx.Tx) ([]byte, error) {
			var state TransactionState
			if err := tx.QueryRow(ctx, `
				SELECT state FROM ledger_transactions WHERE id = $1 FOR UPDATE
			`, originalTransactionID).Scan(&state); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrTransactionNotFound
				}
				return nil, fmt.Errorf("ledger: lock original transaction: %w", err)
			}
			if !state.CanTransitionTo(TransactionStateReversed) {
				return nil, ErrNotReversible
			}

			rows, err := tx.Query(ctx, `
				SELECT account_id::text, amount_minor, currency FROM ledger_entries WHERE transaction_id = $1
			`, originalTransactionID)
			if err != nil {
				return nil, fmt.Errorf("ledger: fetch original entries: %w", err)
			}
			var legs []entryLeg
			for rows.Next() {
				var leg entryLeg
				if err := rows.Scan(&leg.AccountID, &leg.Amount.Minor, &leg.Amount.Currency); err != nil {
					rows.Close()
					return nil, fmt.Errorf("ledger: scan original entry: %w", err)
				}
				legs = append(legs, entryLeg{AccountID: leg.AccountID, Amount: leg.Amount.Negate()})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("ledger: iterate original entries: %w", err)
			}
			if len(legs) == 0 {
				return nil, ErrTransactionNotFound
			}

			txnID, err := insertPostedTransaction(ctx, tx, TransactionTypeReversal, idempotencyKey, legs)
			if err != nil {
				return nil, err
			}

			if _, err := tx.Exec(ctx, `
				UPDATE ledger_transactions SET state = 'reversed' WHERE id = $1
			`, originalTransactionID); err != nil {
				return nil, fmt.Errorf("ledger: mark original reversed: %w", err)
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
		Type:           TransactionTypeReversal,
		State:          TransactionState(result.State),
	}, nil
}

func hashReversal(originalTransactionID string) string {
	return "reversal:" + originalTransactionID
}
