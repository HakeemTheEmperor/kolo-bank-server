package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/idempotency"
)

// SystemAccountNGN is the per-currency contra/suspense account that absorbs
// the offsetting leg of single-sided Credit/Debit calls, until Phase 4
// replaces it with real external-rail settlement accounts. See
// db/migrations/00007_seed_system_accounts.sql.
const SystemAccountNGN = "00000000-0000-0000-0000-000000000001"

// idempotencyTTL bounds how long a completed idempotency record is honored
// before it can be reused; well past any reasonable client retry window.
const idempotencyTTL = 24 * time.Hour

const (
	errCodeInsufficientBalance = "B0001"
	errCodeAccountNotFound     = "B0002"
	errCodeUnbalancedTxn       = "B0003"
)

// Service implements the ledger's core money-movement operations. Every
// mutating method posts balanced double-entry rows atomically and is safe
// to retry under the same idempotency key (docs/banking-backend-spec.md §6).
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// entryLeg is one side of a balanced posting.
type entryLeg struct {
	AccountID string
	Amount    Money
}

// transactionResult is the idempotency response payload persisted for
// replay on retried/duplicate requests.
type transactionResult struct {
	TransactionID string `json:"transaction_id"`
	State         string `json:"state"`
}

func (s *Service) OpenAccount(ctx context.Context, ownerID string, accType AccountType, currency string, overdraftLimitMinor int64) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (owner_id, type, currency, overdraft_limit_minor)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, owner_id::text, type, currency, state, overdraft_limit_minor, created_at, updated_at
	`, ownerID, accType, currency, overdraftLimitMinor).Scan(
		&a.ID, &a.OwnerID, &a.Type, &a.Currency, &a.State, &a.OverdraftLimitMinor, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return Account{}, fmt.Errorf("ledger: open account: %w", err)
	}
	return a, nil
}

func (s *Service) GetAccount(ctx context.Context, accountID string) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, owner_id::text, type, currency, state, overdraft_limit_minor, created_at, updated_at
		FROM accounts WHERE id = $1
	`, accountID).Scan(
		&a.ID, &a.OwnerID, &a.Type, &a.Currency, &a.State, &a.OverdraftLimitMinor, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrAccountNotFound
		}
		return Account{}, fmt.Errorf("ledger: get account: %w", err)
	}
	return a, nil
}

// TransitionAccountState moves an account to target, enforcing the account
// lifecycle state machine.
func (s *Service) TransitionAccountState(ctx context.Context, accountID string, target AccountState) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current AccountState
	if err := tx.QueryRow(ctx, `SELECT state FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("ledger: lock account: %w", err)
	}

	if !current.CanTransitionTo(target) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, target)
	}

	if _, err := tx.Exec(ctx, `UPDATE accounts SET state = $2 WHERE id = $1`, accountID, target); err != nil {
		return fmt.Errorf("ledger: update account state: %w", err)
	}

	return tx.Commit(ctx)
}

// GetBalance derives all three balance views from ledger_entries and holds;
// none of them are stored as mutable state.
func (s *Service) GetBalance(ctx context.Context, accountID string) (Balance, error) {
	account, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return Balance{}, err
	}

	var ledgerMinor, pendingMinor int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries WHERE account_id = $1`,
		accountID,
	).Scan(&ledgerMinor); err != nil {
		return Balance{}, fmt.Errorf("ledger: sum ledger entries: %w", err)
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_minor), 0) FROM holds WHERE account_id = $1 AND status = 'active'`,
		accountID,
	).Scan(&pendingMinor); err != nil {
		return Balance{}, fmt.Errorf("ledger: sum active holds: %w", err)
	}

	return Balance{
		Ledger:    Money{Minor: ledgerMinor, Currency: account.Currency},
		Pending:   Money{Minor: pendingMinor, Currency: account.Currency},
		Available: Money{Minor: ledgerMinor - pendingMinor, Currency: account.Currency},
	}, nil
}

// Credit posts funds into accountID, offset against the system account for
// its currency (see SystemAccountNGN).
func (s *Service) Credit(ctx context.Context, accountID string, amount Money, idempotencyKey string) (Transaction, error) {
	systemAccount, err := systemAccountFor(amount.Currency)
	if err != nil {
		return Transaction{}, err
	}
	return s.postTransaction(ctx, TransactionTypeCredit, idempotencyKey, []entryLeg{
		{AccountID: accountID, Amount: amount},
		{AccountID: systemAccount, Amount: amount.Negate()},
	})
}

// Debit removes funds from accountID, offset against the system account for
// its currency (see SystemAccountNGN).
func (s *Service) Debit(ctx context.Context, accountID string, amount Money, idempotencyKey string) (Transaction, error) {
	systemAccount, err := systemAccountFor(amount.Currency)
	if err != nil {
		return Transaction{}, err
	}
	return s.postTransaction(ctx, TransactionTypeDebit, idempotencyKey, []entryLeg{
		{AccountID: accountID, Amount: amount.Negate()},
		{AccountID: systemAccount, Amount: amount},
	})
}

// Transfer moves funds between two accounts of the same currency, instant
// and on-platform (docs/banking-backend-spec.md §3.4).
func (s *Service) Transfer(ctx context.Context, fromAccountID, toAccountID string, amount Money, idempotencyKey string) (Transaction, error) {
	return s.postTransaction(ctx, TransactionTypeTransfer, idempotencyKey, []entryLeg{
		{AccountID: fromAccountID, Amount: amount.Negate()},
		{AccountID: toAccountID, Amount: amount},
	})
}

func systemAccountFor(currency string) (string, error) {
	switch currency {
	case "NGN":
		return SystemAccountNGN, nil
	default:
		return "", fmt.Errorf("ledger: no system account configured for currency %q", currency)
	}
}

func (s *Service) postTransaction(ctx context.Context, txType TransactionType, idempotencyKey string, legs []entryLeg) (Transaction, error) {
	hash := hashLegs(txType, legs)

	respBytes, err := idempotency.Execute(ctx, s.pool, idempotencyKey, hash, idempotencyTTL,
		func(ctx context.Context, tx pgx.Tx) ([]byte, error) {
			txnID, err := insertPostedTransaction(ctx, tx, txType, idempotencyKey, legs)
			if err != nil {
				return nil, err
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
		Type:           txType,
		State:          TransactionState(result.State),
	}, nil
}

func hashLegs(txType TransactionType, legs []entryLeg) string {
	h := sha256.New()
	h.Write([]byte(txType))
	for _, leg := range legs {
		h.Write([]byte(leg.AccountID))
		h.Write([]byte(fmt.Sprintf(":%d:%s;", leg.Amount.Minor, leg.Amount.Currency)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func translatePostingError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case errCodeInsufficientBalance:
			return ErrInsufficientBalance
		case errCodeAccountNotFound:
			return ErrAccountNotFound
		case errCodeUnbalancedTxn:
			return fmt.Errorf("ledger: unbalanced transaction: %w", err)
		}
	}
	return fmt.Errorf("ledger: post entry: %w", err)
}
