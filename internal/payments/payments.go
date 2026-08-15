// Package payments is the policy layer over internal transfers
// (docs/banking-backend-spec.md §3.4): KYC-tier transaction limits and
// P2P recipient resolution. The ledger itself (internal/ledger) enforces
// only hard invariants (balance, double-entry) and knows nothing about
// identities or KYC — that separation is deliberate, mirroring the
// identity/ledger split already established in Phase 1-2.
package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

// ErrLimitExceeded is returned when a transfer would exceed the sender's
// KYC-tier single-transaction or daily cumulative limit.
var ErrLimitExceeded = errors.New("payments: transfer exceeds tier limit")

// ErrRecipientNotFound is returned when a P2P transfer's recipient email
// doesn't resolve to an active identity with an open account.
var ErrRecipientNotFound = errors.New("payments: recipient not found")

// Limits are minor-unit ceilings for a KYC tier.
type Limits struct {
	MaxSingleMinor int64
	MaxDailyMinor  int64
}

// tierLimits is deliberately conservative at tier 0 (unverified — can
// receive but barely send) and opens up as KYC verification deepens,
// matching docs/banking-backend-spec.md §3.1's "tiered account limits by
// verification level." Same table-driven style as identity.roleScopes
// (internal/identity/identity.go).
var tierLimits = map[int]Limits{
	0: {MaxSingleMinor: 5_000_00, MaxDailyMinor: 5_000_00},           // ₦5,000
	1: {MaxSingleMinor: 500_000_00, MaxDailyMinor: 1_000_000_00},     // ₦500k / ₦1m
	2: {MaxSingleMinor: 10_000_000_00, MaxDailyMinor: 20_000_000_00}, // ₦10m / ₦20m
}

type Service struct {
	pool        *pgxpool.Pool
	ledgerSvc   *ledger.Service
	identitySvc *identity.Service
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service, identitySvc *identity.Service) *Service {
	return &Service{pool: pool, ledgerSvc: ledgerSvc, identitySvc: identitySvc}
}

// Transfer moves funds between two accounts, enforcing the sending
// identity's KYC-tier limits before delegating to the ledger.
func (s *Service) Transfer(ctx context.Context, fromAccountID, toAccountID string, amount ledger.Money, idempotencyKey string) (ledger.Transaction, error) {
	if err := s.checkLimits(ctx, fromAccountID, amount.Minor); err != nil {
		return ledger.Transaction{}, err
	}
	return s.ledgerSvc.Transfer(ctx, fromAccountID, toAccountID, amount, idempotencyKey)
}

// SendToRecipientEmail resolves recipientEmail to an identity and their
// earliest open account (no "primary account" concept yet — a documented
// simplification), then transfers to it.
func (s *Service) SendToRecipientEmail(ctx context.Context, fromAccountID, recipientEmail string, amount ledger.Money, idempotencyKey string) (ledger.Transaction, error) {
	recipient, err := s.identitySvc.GetByEmail(ctx, recipientEmail)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return ledger.Transaction{}, ErrRecipientNotFound
		}
		return ledger.Transaction{}, err
	}

	var toAccountID string
	err = s.pool.QueryRow(ctx, `
		SELECT id::text FROM accounts WHERE owner_id = $1 AND state = 'open' ORDER BY created_at LIMIT 1
	`, recipient.ID).Scan(&toAccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ledger.Transaction{}, ErrRecipientNotFound
		}
		return ledger.Transaction{}, fmt.Errorf("payments: find recipient account: %w", err)
	}

	return s.Transfer(ctx, fromAccountID, toAccountID, amount, idempotencyKey)
}

// Reverse reverses a previously posted transaction.
func (s *Service) Reverse(ctx context.Context, transactionID, idempotencyKey string) (ledger.Transaction, error) {
	return s.ledgerSvc.ReverseTransaction(ctx, transactionID, idempotencyKey)
}

// CheckLimits exposes the sending identity's KYC-tier limit check for
// callers that need to enforce it without going through Transfer itself —
// internal/coolingoff, whose high-risk path defers the actual ledger
// movement, still enforces the same limits at hold-placement time.
func (s *Service) CheckLimits(ctx context.Context, fromAccountID string, amountMinor int64) error {
	return s.checkLimits(ctx, fromAccountID, amountMinor)
}

func (s *Service) checkLimits(ctx context.Context, fromAccountID string, amountMinor int64) error {
	var (
		ownerID string
		tier    int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT i.id::text, i.kyc_tier
		FROM accounts a
		JOIN identities i ON i.id = a.owner_id
		WHERE a.id = $1
	`, fromAccountID).Scan(&ownerID, &tier)
	if err != nil {
		return fmt.Errorf("payments: look up sender tier: %w", err)
	}

	limits, ok := tierLimits[tier]
	if !ok {
		return fmt.Errorf("payments: no limits configured for tier %d", tier)
	}

	if amountMinor > limits.MaxSingleMinor {
		return fmt.Errorf("%w: %d exceeds single-transfer limit %d for tier %d", ErrLimitExceeded, amountMinor, limits.MaxSingleMinor, tier)
	}

	var sentTodayMinor int64
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(-le.amount_minor), 0)
		FROM ledger_entries le
		JOIN ledger_transactions lt ON lt.id = le.transaction_id
		JOIN accounts a ON a.id = le.account_id
		WHERE a.owner_id = $1
		  AND lt.type = 'transfer'
		  AND le.amount_minor < 0
		  AND le.created_at >= date_trunc('day', now())
	`, ownerID).Scan(&sentTodayMinor)
	if err != nil {
		return fmt.Errorf("payments: sum today's transfers: %w", err)
	}

	if sentTodayMinor+amountMinor > limits.MaxDailyMinor {
		return fmt.Errorf("%w: today's total %d + %d exceeds daily limit %d for tier %d", ErrLimitExceeded, sentTodayMinor, amountMinor, limits.MaxDailyMinor, tier)
	}

	return nil
}

// TierLimits exposes the configured limits for a tier, for callers that
// need to display or reason about them without duplicating the table.
func TierLimits(tier int) (Limits, bool) {
	l, ok := tierLimits[tier]
	return l, ok
}
