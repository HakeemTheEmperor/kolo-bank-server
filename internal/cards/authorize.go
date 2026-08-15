package cards

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

var (
	// ErrNotAuthorized is returned when an action requires an authorization
	// in a state it isn't currently in (e.g. completing 3DS on an already-
	// resolved authorization, settling one that was never approved).
	ErrNotAuthorized = errors.New("cards: authorization is not in the required state")
)

type AuthStatus string

const (
	AuthStatusPending      AuthStatus = "pending"
	AuthStatusRequires3DS  AuthStatus = "requires_3ds"
	AuthStatusApproved     AuthStatus = "approved"
	AuthStatusDeclined     AuthStatus = "declined"
	AuthStatusSettled      AuthStatus = "settled"
	AuthStatusVoided       AuthStatus = "voided"
	AuthStatusChargedBack  AuthStatus = "charged_back"
)

type Authorization struct {
	ID                  string
	CardID              string
	AccountID           string
	MerchantName        string
	MCC                 string
	Amount              ledger.Money
	Status              AuthStatus
	DeclineReason       string
	HoldID              *string
	LedgerTransactionID *string
	CreatedAt           time.Time
	SettledAt           *time.Time
}

// Authorize evaluates a card-not-present-style authorization request the
// way an issuer processor would: card status, merchant-category block,
// per-transaction and daily limits, then the (stubbed) network. A 3DS
// threshold hit leaves the authorization requires_3ds with no hold placed;
// otherwise approval places a hold immediately.
func (s *Service) Authorize(ctx context.Context, cardID, merchantName, mcc string, amount ledger.Money, idempotencyKey string) (Authorization, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, idempotencyKey); err != nil {
		return Authorization{}, err
	} else if ok {
		return existing, nil
	}

	card, err := s.Get(ctx, cardID)
	if err != nil {
		return Authorization{}, err
	}
	limits, err := s.GetLimits(ctx, cardID)
	if err != nil {
		return Authorization{}, err
	}

	decision, reason := s.evaluate(ctx, card, limits, merchantName, mcc, amount)

	switch decision {
	case decisionDecline:
		return s.recordDeclined(ctx, card, merchantName, mcc, amount, reason, idempotencyKey)
	case decisionRequires3DS:
		return s.recordRequires3DS(ctx, card, merchantName, mcc, amount, idempotencyKey)
	default: // decisionApprove
		return s.recordApproved(ctx, card, merchantName, mcc, amount, idempotencyKey)
	}
}

type decision int

const (
	decisionApprove decision = iota
	decisionDecline
	decisionRequires3DS
)

func (s *Service) evaluate(ctx context.Context, card Card, limits Limits, merchantName, mcc string, amount ledger.Money) (decision, string) {
	if card.Status != StatusActive {
		return decisionDecline, "card_inactive"
	}
	for _, blocked := range limits.BlockedMCCs {
		if blocked == mcc {
			return decisionDecline, "mcc_blocked"
		}
	}
	if amount.Minor > limits.PerTransactionLimitMinor {
		return decisionDecline, "limit_exceeded"
	}

	spentToday, err := s.approvedTodaySum(ctx, card.ID)
	if err != nil {
		return decisionDecline, "internal_error"
	}
	if spentToday+amount.Minor > limits.DailyLimitMinor {
		return decisionDecline, "daily_limit_exceeded"
	}

	if approved, reason := simulateNetwork(merchantName); !approved {
		return decisionDecline, reason
	}

	if amount.Minor >= threeDSThresholdMinor {
		return decisionRequires3DS, ""
	}
	return decisionApprove, ""
}

func (s *Service) approvedTodaySum(ctx context.Context, cardID string) (int64, error) {
	var sum int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor), 0) FROM card_authorizations
		WHERE card_id = $1 AND status IN ('approved', 'settled') AND created_at >= date_trunc('day', now())
	`, cardID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("cards: sum today's approvals: %w", err)
	}
	return sum, nil
}

func (s *Service) recordDeclined(ctx context.Context, card Card, merchantName, mcc string, amount ledger.Money, reason, idempotencyKey string) (Authorization, error) {
	return s.insert(ctx, card, merchantName, mcc, amount, AuthStatusDeclined, reason, nil, idempotencyKey)
}

func (s *Service) recordApproved(ctx context.Context, card Card, merchantName, mcc string, amount ledger.Money, idempotencyKey string) (Authorization, error) {
	return s.placeHoldAndRecord(ctx, card, merchantName, mcc, amount, idempotencyKey)
}

func (s *Service) recordRequires3DS(ctx context.Context, card Card, merchantName, mcc string, amount ledger.Money, idempotencyKey string) (Authorization, error) {
	auth, err := s.insert(ctx, card, merchantName, mcc, amount, AuthStatusRequires3DS, "", nil, idempotencyKey)
	if err != nil {
		return Authorization{}, err
	}
	if err := s.issueThreeDSChallenge(ctx, auth.ID); err != nil {
		return Authorization{}, err
	}
	return auth, nil
}

// placeHoldAndRecord requires the ledger service, threaded through the
// package-level ledgerSvc field set at construction.
func (s *Service) placeHoldAndRecord(ctx context.Context, card Card, merchantName, mcc string, amount ledger.Money, idempotencyKey string) (Authorization, error) {
	hold, err := s.ledgerSvc.PlaceHold(ctx, card.AccountID, amount)
	if err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			return s.insert(ctx, card, merchantName, mcc, amount, AuthStatusDeclined, "insufficient_funds", nil, idempotencyKey)
		}
		return Authorization{}, fmt.Errorf("cards: place hold: %w", err)
	}
	return s.insert(ctx, card, merchantName, mcc, amount, AuthStatusApproved, "", &hold.ID, idempotencyKey)
}

const authColumns = `id::text, card_id::text, account_id::text, merchant_name, mcc, amount_minor, currency, status, decline_reason, hold_id::text, ledger_transaction_id::text, created_at, settled_at`

// authScanner is satisfied by both pgx.Row (QueryRow) and *pgx.Rows
// (Query, per-row via Next/Scan) — decline_reason is nullable, so both
// scan paths go through the same *string intermediate.
type authScanner interface {
	Scan(dest ...any) error
}

func scanAuth(row authScanner) (Authorization, error) {
	var a Authorization
	var declineReason *string
	err := row.Scan(
		&a.ID, &a.CardID, &a.AccountID, &a.MerchantName, &a.MCC, &a.Amount.Minor, &a.Amount.Currency, &a.Status, &declineReason, &a.HoldID, &a.LedgerTransactionID, &a.CreatedAt, &a.SettledAt,
	)
	if err != nil {
		return Authorization{}, err
	}
	if declineReason != nil {
		a.DeclineReason = *declineReason
	}
	return a, nil
}

func (s *Service) insert(ctx context.Context, card Card, merchantName, mcc string, amount ledger.Money, status AuthStatus, reason string, holdID *string, idempotencyKey string) (Authorization, error) {
	var declineReason *string
	if reason != "" {
		declineReason = &reason
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO card_authorizations (card_id, account_id, merchant_name, mcc, amount_minor, currency, status, decline_reason, hold_id, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+authColumns, card.ID, card.AccountID, merchantName, mcc, amount.Minor, amount.Currency, status, declineReason, holdID, idempotencyKey)
	a, err := scanAuth(row)
	if err != nil {
		return Authorization{}, fmt.Errorf("cards: record authorization: %w", err)
	}
	return a, nil
}

func (s *Service) getByIdempotencyKey(ctx context.Context, idempotencyKey string) (Authorization, bool, error) {
	a, err := s.loadAuth(ctx, `idempotency_key = $1`, idempotencyKey)
	if err != nil {
		if errors.Is(err, ErrNotAuthorized) {
			return Authorization{}, false, nil
		}
		return Authorization{}, false, err
	}
	return a, true, nil
}

func (s *Service) loadAuth(ctx context.Context, where string, arg any) (Authorization, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+authColumns+` FROM card_authorizations WHERE `+where, arg)
	a, err := scanAuth(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Authorization{}, ErrNotAuthorized
		}
		return Authorization{}, fmt.Errorf("cards: load authorization: %w", err)
	}
	return a, nil
}

// GetAuthorization returns a card authorization by id.
func (s *Service) GetAuthorization(ctx context.Context, authID string) (Authorization, error) {
	return s.loadAuth(ctx, `id = $1`, authID)
}

// ListAuthorizationsByCard returns cardID's authorizations, newest first.
func (s *Service) ListAuthorizationsByCard(ctx context.Context, cardID string) ([]Authorization, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+authColumns+`
		FROM card_authorizations WHERE card_id = $1
		ORDER BY created_at DESC
	`, cardID)
	if err != nil {
		return nil, fmt.Errorf("cards: list authorizations: %w", err)
	}
	defer rows.Close()

	var out []Authorization
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			return nil, fmt.Errorf("cards: scan authorization: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Complete3DS verifies code against authID's stub challenge. Success
// places the hold and marks the authorization approved; failure declines
// it outright — one attempt, no retry loop, matching a real issuer's
// "you get one shot at the OTP" behavior closely enough for this build.
func (s *Service) Complete3DS(ctx context.Context, authID, code string) (Authorization, error) {
	auth, err := s.GetAuthorization(ctx, authID)
	if err != nil {
		return Authorization{}, err
	}
	if auth.Status != AuthStatusRequires3DS {
		return Authorization{}, ErrNotAuthorized
	}

	var storedHash string
	var expiresAt time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT threeds_code_hash, threeds_expires_at FROM card_authorizations WHERE id = $1
	`, authID).Scan(&storedHash, &expiresAt); err != nil {
		return Authorization{}, fmt.Errorf("cards: load 3ds challenge: %w", err)
	}

	if time.Now().After(expiresAt) || !verifyThreeDSCode(storedHash, code) {
		if _, err := s.pool.Exec(ctx, `
			UPDATE card_authorizations SET status = 'declined', decline_reason = '3ds_failed' WHERE id = $1
		`, authID); err != nil {
			return Authorization{}, fmt.Errorf("cards: mark 3ds declined: %w", err)
		}
		return s.GetAuthorization(ctx, authID)
	}

	card, err := s.Get(ctx, auth.CardID)
	if err != nil {
		return Authorization{}, err
	}

	hold, err := s.ledgerSvc.PlaceHold(ctx, card.AccountID, auth.Amount)
	if err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			if _, err := s.pool.Exec(ctx, `
				UPDATE card_authorizations SET status = 'declined', decline_reason = 'insufficient_funds' WHERE id = $1
			`, authID); err != nil {
				return Authorization{}, fmt.Errorf("cards: mark declined: %w", err)
			}
			return s.GetAuthorization(ctx, authID)
		}
		return Authorization{}, fmt.Errorf("cards: place hold after 3ds: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE card_authorizations SET status = 'approved', hold_id = $2 WHERE id = $1
	`, authID, hold.ID); err != nil {
		return Authorization{}, fmt.Errorf("cards: mark approved: %w", err)
	}
	return s.GetAuthorization(ctx, authID)
}

// Settle captures an approved authorization's hold into a real posted
// ledger transaction.
func (s *Service) Settle(ctx context.Context, authID string) (Authorization, error) {
	auth, err := s.GetAuthorization(ctx, authID)
	if err != nil {
		return Authorization{}, err
	}
	if auth.Status != AuthStatusApproved || auth.HoldID == nil {
		return Authorization{}, ErrNotAuthorized
	}

	txn, err := s.ledgerSvc.CaptureHold(ctx, *auth.HoldID, "settle:"+authID)
	if err != nil {
		return Authorization{}, fmt.Errorf("cards: capture hold: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE card_authorizations SET status = 'settled', ledger_transaction_id = $2, settled_at = now() WHERE id = $1
	`, authID, txn.ID); err != nil {
		return Authorization{}, fmt.Errorf("cards: mark settled: %w", err)
	}
	return s.GetAuthorization(ctx, authID)
}

// Void releases an approved, not-yet-settled authorization's hold —
// a purchase cancelled before settlement.
func (s *Service) Void(ctx context.Context, authID string) error {
	auth, err := s.GetAuthorization(ctx, authID)
	if err != nil {
		return err
	}
	if auth.Status != AuthStatusApproved || auth.HoldID == nil {
		return ErrNotAuthorized
	}

	if err := s.ledgerSvc.ReleaseHold(ctx, *auth.HoldID); err != nil {
		return fmt.Errorf("cards: release hold: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE card_authorizations SET status = 'voided' WHERE id = $1`, authID); err != nil {
		return fmt.Errorf("cards: mark voided: %w", err)
	}
	return nil
}

// Chargeback reverses a settled authorization's posted transaction — the
// fund-reversal action available once a dispute over a card purchase
// resolves in the cardholder's favor (docs/banking-backend-spec.md §3.5:
// "chargeback/dispute handling tied into Phase 8 disputes"). Deliberately
// a standalone, explicitly-called action rather than an automatic side
// effect of disputes.Advance — see internal/cards package doc.
func (s *Service) Chargeback(ctx context.Context, authID, idempotencyKey string) (Authorization, error) {
	auth, err := s.GetAuthorization(ctx, authID)
	if err != nil {
		return Authorization{}, err
	}
	if auth.Status == AuthStatusChargedBack {
		return auth, nil
	}
	if auth.Status != AuthStatusSettled || auth.LedgerTransactionID == nil {
		return Authorization{}, ErrNotAuthorized
	}

	if _, err := s.ledgerSvc.ReverseTransaction(ctx, *auth.LedgerTransactionID, idempotencyKey); err != nil {
		return Authorization{}, fmt.Errorf("cards: reverse transaction: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE card_authorizations SET status = 'charged_back' WHERE id = $1 AND status = 'settled'
	`, authID); err != nil {
		return Authorization{}, fmt.Errorf("cards: mark charged back: %w", err)
	}
	return s.GetAuthorization(ctx, authID)
}
