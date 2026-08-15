// Package checkout implements a backend-only hosted-checkout-session
// resource (docs/banking-backend-spec.md §3.6). It deliberately does not
// serve any HTML: a real frontend would fetch the session and render the
// payment page — building that frontend is a non-goal for this backend
// (§1). RedirectURL is a placeholder documenting where that page would
// live.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

var ErrNotFound = errors.New("checkout: session not found")

type Status string

const (
	StatusOpen      Status = "open"
	StatusCompleted Status = "completed"
	StatusExpired   Status = "expired"
)

const sessionTTL = 30 * time.Minute

type Session struct {
	ID             string
	MerchantID     string
	Mode           apikeys.Mode
	Amount         ledger.Money
	SuccessURL     string
	CancelURL      string
	Status         Status
	ChargeID       *string
	IdempotencyKey string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// RedirectURL is where a real hosted frontend would render this session —
// not served by this backend.
func (s Session) RedirectURL(publicBaseURL string) string {
	return fmt.Sprintf("%s/checkout/%s", publicBaseURL, s.ID)
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Create(ctx context.Context, merchantID string, mode apikeys.Mode, amount ledger.Money, successURL, cancelURL, idempotencyKey string) (Session, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, merchantID, idempotencyKey); err != nil {
		return Session{}, err
	} else if ok {
		return existing, nil
	}

	var sess Session
	err := s.pool.QueryRow(ctx, `
		INSERT INTO checkout_sessions (merchant_id, mode, amount_minor, currency, success_url, cancel_url, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + $8)
		RETURNING id::text, merchant_id::text, mode, amount_minor, currency, success_url, cancel_url, status, charge_id::text, idempotency_key, created_at, expires_at
	`, merchantID, mode, amount.Minor, amount.Currency, successURL, cancelURL, idempotencyKey, sessionTTL).Scan(
		&sess.ID, &sess.MerchantID, &sess.Mode, &sess.Amount.Minor, &sess.Amount.Currency, &sess.SuccessURL, &sess.CancelURL,
		&sess.Status, &sess.ChargeID, &sess.IdempotencyKey, &sess.CreatedAt, &sess.ExpiresAt,
	)
	if err != nil {
		return Session{}, fmt.Errorf("checkout: create: %w", err)
	}
	return sess, nil
}

func (s *Service) Get(ctx context.Context, id string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, amount_minor, currency, success_url, cancel_url, status, charge_id::text, idempotency_key, created_at, expires_at
		FROM checkout_sessions WHERE id = $1
	`, id).Scan(
		&sess.ID, &sess.MerchantID, &sess.Mode, &sess.Amount.Minor, &sess.Amount.Currency, &sess.SuccessURL, &sess.CancelURL,
		&sess.Status, &sess.ChargeID, &sess.IdempotencyKey, &sess.CreatedAt, &sess.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("checkout: get: %w", err)
	}
	return sess, nil
}

func (s *Service) getByIdempotencyKey(ctx context.Context, merchantID, idempotencyKey string) (Session, bool, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, amount_minor, currency, success_url, cancel_url, status, charge_id::text, idempotency_key, created_at, expires_at
		FROM checkout_sessions WHERE merchant_id = $1 AND idempotency_key = $2
	`, merchantID, idempotencyKey).Scan(
		&sess.ID, &sess.MerchantID, &sess.Mode, &sess.Amount.Minor, &sess.Amount.Currency, &sess.SuccessURL, &sess.CancelURL,
		&sess.Status, &sess.ChargeID, &sess.IdempotencyKey, &sess.CreatedAt, &sess.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("checkout: lookup by idempotency key: %w", err)
	}
	return sess, true, nil
}
