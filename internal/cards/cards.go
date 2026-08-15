// Package cards implements card issuing and controls
// (docs/banking-backend-spec.md §3.5): virtual and physical cards tied to
// a customer's own account, freeze/unfreeze, per-card daily and
// per-transaction limits, and merchant-category blocks. Authorization and
// settlement (authorize.go) compose ledger.Service.PlaceHold/CaptureHold/
// ReleaseHold exactly as designed — a card purchase is money leaving the
// bank's ledger the same way an outbound external transfer is
// (internal/externalpayments), so no new money-movement primitive is
// needed here.
package cards

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

var (
	// ErrNotFound is returned when a card doesn't exist, or doesn't belong
	// to the calling identity.
	ErrNotFound = errors.New("cards: not found")
)

type CardType string

const (
	CardTypeVirtual  CardType = "virtual"
	CardTypePhysical CardType = "physical"
)

type Status string

const (
	StatusActive Status = "active"
	StatusFrozen Status = "frozen"
	StatusClosed Status = "closed"
)

type Card struct {
	ID         string
	IdentityID string
	AccountID  string
	CardType   CardType
	PANLast4   string
	Brand      string
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Limits struct {
	DailyLimitMinor         int64
	PerTransactionLimitMinor int64
	BlockedMCCs             []string
}

// defaultLimits are conservative starting limits a cardholder can raise
// via SetLimits — same "safe default, customer can adjust" spirit as
// internal/payments' tierLimits.
var defaultLimits = Limits{
	DailyLimitMinor:          500_000_00,
	PerTransactionLimitMinor: 200_000_00,
}

type Service struct {
	pool      *pgxpool.Pool
	ledgerSvc *ledger.Service
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service) *Service {
	return &Service{pool: pool, ledgerSvc: ledgerSvc}
}

// Issue creates a new card for identityID against accountID, with default
// limits. pan_last4 is generated locally (no real PAN exists to derive it
// from — this is a stub issuer, not a real card personalization service).
func (s *Service) Issue(ctx context.Context, identityID, accountID string, cardType CardType) (Card, error) {
	last4, err := randomDigits4()
	if err != nil {
		return Card{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Card{}, fmt.Errorf("cards: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c Card
	err = tx.QueryRow(ctx, `
		INSERT INTO cards (identity_id, account_id, card_type, pan_last4)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, identity_id::text, account_id::text, card_type, pan_last4, brand, status, created_at, updated_at
	`, identityID, accountID, cardType, last4).Scan(
		&c.ID, &c.IdentityID, &c.AccountID, &c.CardType, &c.PANLast4, &c.Brand, &c.Status, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return Card{}, fmt.Errorf("cards: issue: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO card_limits (card_id, daily_limit_minor, per_transaction_limit_minor, blocked_mccs)
		VALUES ($1, $2, $3, '{}')
	`, c.ID, defaultLimits.DailyLimitMinor, defaultLimits.PerTransactionLimitMinor); err != nil {
		return Card{}, fmt.Errorf("cards: create default limits: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Card{}, fmt.Errorf("cards: commit: %w", err)
	}
	return c, nil
}

// Freeze/Unfreeze toggle a card between active and frozen. Ownership is
// the caller's responsibility to check (internal/publicapi does, the same
// pattern as ownsAPIKey).
func (s *Service) Freeze(ctx context.Context, cardID string) error {
	return s.setStatus(ctx, cardID, StatusFrozen)
}

func (s *Service) Unfreeze(ctx context.Context, cardID string) error {
	return s.setStatus(ctx, cardID, StatusActive)
}

func (s *Service) setStatus(ctx context.Context, cardID string, status Status) error {
	tag, err := s.pool.Exec(ctx, `UPDATE cards SET status = $2 WHERE id = $1 AND status != 'closed'`, cardID, status)
	if err != nil {
		return fmt.Errorf("cards: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetLimits replaces cardID's daily limit, per-transaction limit, and
// merchant-category block list.
func (s *Service) SetLimits(ctx context.Context, cardID string, dailyLimitMinor, perTransactionLimitMinor int64, blockedMCCs []string) error {
	if blockedMCCs == nil {
		blockedMCCs = []string{}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE card_limits SET daily_limit_minor = $2, per_transaction_limit_minor = $3, blocked_mccs = $4
		WHERE card_id = $1
	`, cardID, dailyLimitMinor, perTransactionLimitMinor, blockedMCCs)
	if err != nil {
		return fmt.Errorf("cards: set limits: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Get(ctx context.Context, cardID string) (Card, error) {
	var c Card
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, identity_id::text, account_id::text, card_type, pan_last4, brand, status, created_at, updated_at
		FROM cards WHERE id = $1
	`, cardID).Scan(&c.ID, &c.IdentityID, &c.AccountID, &c.CardType, &c.PANLast4, &c.Brand, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Card{}, ErrNotFound
		}
		return Card{}, fmt.Errorf("cards: get: %w", err)
	}
	return c, nil
}

// GetLimits returns cardID's current limits.
func (s *Service) GetLimits(ctx context.Context, cardID string) (Limits, error) {
	var l Limits
	err := s.pool.QueryRow(ctx, `
		SELECT daily_limit_minor, per_transaction_limit_minor, blocked_mccs FROM card_limits WHERE card_id = $1
	`, cardID).Scan(&l.DailyLimitMinor, &l.PerTransactionLimitMinor, &l.BlockedMCCs)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Limits{}, ErrNotFound
		}
		return Limits{}, fmt.Errorf("cards: get limits: %w", err)
	}
	return l, nil
}

// ListByIdentity returns identityID's cards, newest first.
func (s *Service) ListByIdentity(ctx context.Context, identityID string) ([]Card, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, identity_id::text, account_id::text, card_type, pan_last4, brand, status, created_at, updated_at
		FROM cards WHERE identity_id = $1
		ORDER BY created_at DESC
	`, identityID)
	if err != nil {
		return nil, fmt.Errorf("cards: list: %w", err)
	}
	defer rows.Close()

	var out []Card
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.IdentityID, &c.AccountID, &c.CardType, &c.PANLast4, &c.Brand, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("cards: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func randomDigits4() (string, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cards: generate pan suffix: %w", err)
	}
	n := (int(b[0])<<8 | int(b[1])) % 10000
	return fmt.Sprintf("%04d", n), nil
}
