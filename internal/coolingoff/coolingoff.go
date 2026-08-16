// Package coolingoff implements confirmation of payee and scam
// interruption / cooling-off on high-risk P2P transfers
// (docs/banking-backend-spec.md §5.1, §5.2). It composes existing
// primitives rather than inventing new money-movement mechanics: recipient
// resolution mirrors internal/payments.SendToRecipientEmail, tier-limit
// enforcement reuses payments.Service.CheckLimits, name comparison reuses
// internal/payee, and a high-risk transfer is deferred via
// ledger.Service.PlaceHold/ReleaseHold — the same hold-then-settle
// primitive Phases 3/4/7 already rely on — rather than a bespoke
// reservation mechanism.
package coolingoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payee"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
)

// ErrRecipientNotFound mirrors payments.ErrRecipientNotFound — duplicated
// rather than imported so callers of this package don't need to know
// SendToRecipientEmail's low-risk path is delegated to payments
// internally.
var ErrRecipientNotFound = errors.New("coolingoff: recipient not found")

// ErrPayeeMismatchNotConfirmed is returned when the typed recipient name
// doesn't match the account on record and the caller hasn't set
// confirmMismatch — no funds move until the sender explicitly confirms
// they still want to send. This is what catches a misdirected payment
// pre-send.
var ErrPayeeMismatchNotConfirmed = errors.New("coolingoff: recipient name mismatch not confirmed")

// coolingOffWindow is demo-scale (a real deployment would use minutes to
// hours); it just needs to be long enough for a customer to see and
// cancel a warned transfer, and short enough to verify in a test run.
const coolingOffWindow = 2 * time.Minute

// largeAmountMinor flags a transfer as high-risk regardless of payee
// history — "unusually large" per docs/banking-backend-spec.md §5.2.
const largeAmountMinor = 500_000_00

type Outcome string

const (
	// OutcomeCompleted means the transfer posted immediately (low risk).
	OutcomeCompleted Outcome = "completed"
	// OutcomeHeld means funds are reserved but not yet moved; cancellable
	// until ReleaseAt.
	OutcomeHeld Outcome = "held"
)

type SendResult struct {
	Outcome     Outcome
	PayeeResult payee.Result
	Reasons     []string
	Transaction ledger.Transaction // set when Outcome == OutcomeCompleted
	PendingID   string             // set when Outcome == OutcomeHeld
	ReleaseAt   time.Time          // set when Outcome == OutcomeHeld
}

type PendingTransfer struct {
	ID          string
	ToAccountID string
	AmountMinor int64
	Currency    string
	Reasons     []string
	Status      string
	ReleaseAt   time.Time
	CreatedAt   time.Time
}

type Service struct {
	pool          *pgxpool.Pool
	ledgerSvc     *ledger.Service
	paymentsSvc   *payments.Service
	identitySvc   *identity.Service
	resilienceSvc *resilience.Service
	logger        *slog.Logger
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service, paymentsSvc *payments.Service, identitySvc *identity.Service, resilienceSvc *resilience.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, ledgerSvc: ledgerSvc, paymentsSvc: paymentsSvc, identitySvc: identitySvc, resilienceSvc: resilienceSvc, logger: logger}
}

// Send resolves recipientEmail, checks the typed name against the account
// on record, enforces tier limits, and either settles immediately (low
// risk) or places a cancellable hold (high risk: first-time payee, a
// large amount, or any payee-name mismatch).
func (s *Service) Send(ctx context.Context, fromAccountID, fromIdentityID, recipientEmail, typedRecipientName string, amount ledger.Money, idempotencyKey string, confirmMismatch bool) (SendResult, error) {
	if err := s.resilienceSvc.Check(ctx, resilience.Feature("transfer")); err != nil {
		return SendResult{}, err
	}

	recipient, err := s.identitySvc.GetByEmail(ctx, recipientEmail)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return SendResult{}, ErrRecipientNotFound
		}
		return SendResult{}, err
	}

	var toAccountID string
	err = s.pool.QueryRow(ctx, `
		SELECT id::text FROM accounts WHERE owner_id = $1 AND state = 'open' ORDER BY created_at LIMIT 1
	`, recipient.ID).Scan(&toAccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SendResult{}, ErrRecipientNotFound
		}
		return SendResult{}, fmt.Errorf("coolingoff: find recipient account: %w", err)
	}

	payeeResult := payee.Check(typedRecipientName, recipient.LegalName)
	if payeeResult == payee.NoMatch && !confirmMismatch {
		return SendResult{PayeeResult: payeeResult}, ErrPayeeMismatchNotConfirmed
	}

	if err := s.paymentsSvc.CheckLimits(ctx, fromAccountID, amount.Minor); err != nil {
		return SendResult{}, err
	}

	reasons := riskReasons(payeeResult, amount.Minor)
	firstTime, err := s.isFirstTimePayee(ctx, fromAccountID, toAccountID)
	if err != nil {
		return SendResult{}, err
	}
	if firstTime {
		reasons = append(reasons, "first_time_payee")
	}

	if len(reasons) == 0 {
		txn, err := s.paymentsSvc.Transfer(ctx, fromAccountID, toAccountID, amount, idempotencyKey)
		if err != nil {
			return SendResult{}, err
		}
		return SendResult{Outcome: OutcomeCompleted, PayeeResult: payeeResult, Transaction: txn}, nil
	}

	pending, releaseAt, err := s.placeHold(ctx, fromAccountID, fromIdentityID, toAccountID, recipient.ID, amount, reasons, idempotencyKey)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{
		Outcome:     OutcomeHeld,
		PayeeResult: payeeResult,
		Reasons:     reasons,
		PendingID:   pending,
		ReleaseAt:   releaseAt,
	}, nil
}

func riskReasons(result payee.Result, amountMinor int64) []string {
	var reasons []string
	if result != payee.Match {
		reasons = append(reasons, "payee_"+string(result))
	}
	if amountMinor >= largeAmountMinor {
		reasons = append(reasons, "large_amount")
	}
	return reasons
}

// isFirstTimePayee reports whether fromAccountID has never sent a
// completed internal transfer to toAccountID before.
func (s *Service) isFirstTimePayee(ctx context.Context, fromAccountID, toAccountID string) (bool, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM ledger_entries le
		JOIN ledger_transactions lt ON lt.id = le.transaction_id
		WHERE le.account_id = $1 AND le.amount_minor < 0 AND lt.type = 'transfer' AND lt.state = 'posted'
		  AND EXISTS (
		      SELECT 1 FROM ledger_entries le2
		      WHERE le2.transaction_id = lt.id AND le2.account_id = $2 AND le2.amount_minor > 0
		  )
	`, fromAccountID, toAccountID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("coolingoff: check payee history: %w", err)
	}
	return count == 0, nil
}

func (s *Service) placeHold(ctx context.Context, fromAccountID, fromIdentityID, toAccountID, toIdentityID string, amount ledger.Money, reasons []string, idempotencyKey string) (id string, releaseAt time.Time, err error) {
	hold, err := s.ledgerSvc.PlaceHold(ctx, fromAccountID, amount)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("coolingoff: place hold: %w", err)
	}

	reasonsJSON, err := json.Marshal(reasons)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("coolingoff: marshal reasons: %w", err)
	}

	releaseAt = time.Now().Add(coolingOffWindow)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO cooling_off_transfers (from_account_id, from_identity_id, to_account_id, to_identity_id, amount_minor, currency, reasons, hold_id, release_at, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id::text
	`, fromAccountID, fromIdentityID, toAccountID, toIdentityID, amount.Minor, amount.Currency, reasonsJSON, hold.ID, releaseAt, idempotencyKey).Scan(&id)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("coolingoff: record pending transfer: %w", err)
	}
	return id, releaseAt, nil
}

// Cancel releases a still-pending hold before its cooling-off window
// expires. Ownership-checked: only the sender can cancel their own hold.
func (s *Service) Cancel(ctx context.Context, pendingID, requestingIdentityID string) error {
	var holdID string
	err := s.pool.QueryRow(ctx, `
		SELECT hold_id::text FROM cooling_off_transfers
		WHERE id = $1 AND from_identity_id = $2 AND status = 'pending'
	`, pendingID, requestingIdentityID).Scan(&holdID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPending
		}
		return fmt.Errorf("coolingoff: look up pending transfer: %w", err)
	}

	if err := s.ledgerSvc.ReleaseHold(ctx, holdID); err != nil {
		return fmt.Errorf("coolingoff: release hold: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE cooling_off_transfers SET status = 'cancelled' WHERE id = $1`, pendingID); err != nil {
		return fmt.Errorf("coolingoff: mark cancelled: %w", err)
	}
	return nil
}

// ErrNotPending is returned when cancelling a transfer that either isn't
// this identity's, or has already released/cancelled.
var ErrNotPending = errors.New("coolingoff: transfer is not pending")

// ListPendingByIdentity returns identityID's still-cancellable holds,
// newest first.
func (s *Service) ListPendingByIdentity(ctx context.Context, identityID string) ([]PendingTransfer, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, to_account_id::text, amount_minor, currency, reasons, status, release_at, created_at
		FROM cooling_off_transfers
		WHERE from_identity_id = $1 AND status = 'pending'
		ORDER BY created_at DESC
	`, identityID)
	if err != nil {
		return nil, fmt.Errorf("coolingoff: list pending: %w", err)
	}
	defer rows.Close()

	var out []PendingTransfer
	for rows.Next() {
		var p PendingTransfer
		var reasonsJSON []byte
		if err := rows.Scan(&p.ID, &p.ToAccountID, &p.AmountMinor, &p.Currency, &reasonsJSON, &p.Status, &p.ReleaseAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("coolingoff: scan: %w", err)
		}
		if err := json.Unmarshal(reasonsJSON, &p.Reasons); err != nil {
			return nil, fmt.Errorf("coolingoff: unmarshal reasons: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
