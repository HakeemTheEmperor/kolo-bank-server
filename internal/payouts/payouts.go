// Package payouts implements merchant-initiated payouts
// (docs/banking-backend-spec.md §3.6). Same shape as internal/charges but
// outbound: a thin record over an outbound external transfer, status read
// by joining to external_transfers, execution entirely handled by
// externalpayments' existing tickers.
package payouts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
)

var (
	ErrNoSettlementAccount = errors.New("payouts: merchant has no open settlement account")
	ErrUnknownRail         = errors.New("payouts: unknown rail")
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Payout struct {
	ID                 string
	MerchantID         string
	Mode               apikeys.Mode
	AccountID          string
	RailName           string
	RecipientRef       string
	Amount             ledger.Money
	IdempotencyKey     string
	ExternalTransferID string
	Status             Status
	CreatedAt          time.Time
}

type Service struct {
	pool          *pgxpool.Pool
	externalSvc   *externalpayments.Service
	registry      *rails.Registry
	resilienceSvc *resilience.Service
}

func NewService(pool *pgxpool.Pool, externalSvc *externalpayments.Service, registry *rails.Registry, resilienceSvc *resilience.Service) *Service {
	return &Service{pool: pool, externalSvc: externalSvc, registry: registry, resilienceSvc: resilienceSvc}
}

// Create pays recipientRef via railName from the merchant's settlement
// account. Idempotent per (merchant, idempotencyKey).
func (s *Service) Create(ctx context.Context, merchantID string, mode apikeys.Mode, railName, recipientRef string, amount ledger.Money, idempotencyKey string) (Payout, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, merchantID, idempotencyKey); err != nil {
		return Payout{}, err
	} else if ok {
		return existing, nil
	}

	if err := s.resilienceSvc.Check(ctx, resilience.Feature("payout"), resilience.Merchant(merchantID)); err != nil {
		return Payout{}, err
	}

	if _, err := s.registry.Get(railName); err != nil {
		return Payout{}, ErrUnknownRail
	}

	accountID, err := resolveSettlementAccount(ctx, s.pool, merchantID, amount.Currency)
	if err != nil {
		return Payout{}, err
	}

	et, err := s.externalSvc.SendOutbound(ctx, accountID, railName, recipientRef, amount, idempotencyKey)
	if err != nil {
		return Payout{}, fmt.Errorf("payouts: send outbound: %w", err)
	}

	var p Payout
	err = s.pool.QueryRow(ctx, `
		INSERT INTO payouts (merchant_id, mode, account_id, rail_name, recipient_ref, amount_minor, currency, idempotency_key, external_transfer_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (merchant_id, idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING id::text, merchant_id::text, mode, account_id::text, rail_name, recipient_ref, amount_minor, currency, idempotency_key, external_transfer_id::text, created_at
	`, merchantID, mode, accountID, railName, recipientRef, amount.Minor, amount.Currency, idempotencyKey, et.ID).Scan(
		&p.ID, &p.MerchantID, &p.Mode, &p.AccountID, &p.RailName, &p.RecipientRef, &p.Amount.Minor, &p.Amount.Currency,
		&p.IdempotencyKey, &p.ExternalTransferID, &p.CreatedAt,
	)
	if err != nil {
		return Payout{}, fmt.Errorf("payouts: record: %w", err)
	}
	p.Status = statusFromExternalTransfer(externalpayments.StatusPending)
	return p, nil
}

func (s *Service) Get(ctx context.Context, id string) (Payout, error) {
	var p Payout
	var externalStatus externalpayments.Status
	err := s.pool.QueryRow(ctx, `
		SELECT p.id::text, p.merchant_id::text, p.mode, p.account_id::text, p.rail_name, p.recipient_ref, p.amount_minor, p.currency,
		       p.idempotency_key, p.external_transfer_id::text, p.created_at, et.status
		FROM payouts p JOIN external_transfers et ON et.id = p.external_transfer_id
		WHERE p.id = $1
	`, id).Scan(
		&p.ID, &p.MerchantID, &p.Mode, &p.AccountID, &p.RailName, &p.RecipientRef, &p.Amount.Minor, &p.Amount.Currency,
		&p.IdempotencyKey, &p.ExternalTransferID, &p.CreatedAt, &externalStatus,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Payout{}, fmt.Errorf("payouts: %s: %w", id, pgx.ErrNoRows)
		}
		return Payout{}, fmt.Errorf("payouts: get: %w", err)
	}
	p.Status = statusFromExternalTransfer(externalStatus)
	return p, nil
}

// List returns a merchant's payouts in a mode, newest first — doubles as
// the developer-facing transaction log alongside charges.List.
func (s *Service) List(ctx context.Context, merchantID string, mode apikeys.Mode, limit int) ([]Payout, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.merchant_id::text, p.mode, p.account_id::text, p.rail_name, p.recipient_ref, p.amount_minor, p.currency,
		       p.idempotency_key, p.external_transfer_id::text, p.created_at, et.status
		FROM payouts p JOIN external_transfers et ON et.id = p.external_transfer_id
		WHERE p.merchant_id = $1 AND p.mode = $2
		ORDER BY p.created_at DESC
		LIMIT $3
	`, merchantID, mode, limit)
	if err != nil {
		return nil, fmt.Errorf("payouts: list: %w", err)
	}
	defer rows.Close()

	var out []Payout
	for rows.Next() {
		var p Payout
		var externalStatus externalpayments.Status
		if err := rows.Scan(&p.ID, &p.MerchantID, &p.Mode, &p.AccountID, &p.RailName, &p.RecipientRef, &p.Amount.Minor, &p.Amount.Currency,
			&p.IdempotencyKey, &p.ExternalTransferID, &p.CreatedAt, &externalStatus); err != nil {
			return nil, fmt.Errorf("payouts: scan: %w", err)
		}
		p.Status = statusFromExternalTransfer(externalStatus)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) getByIdempotencyKey(ctx context.Context, merchantID, idempotencyKey string) (Payout, bool, error) {
	var p Payout
	var externalStatus externalpayments.Status
	err := s.pool.QueryRow(ctx, `
		SELECT p.id::text, p.merchant_id::text, p.mode, p.account_id::text, p.rail_name, p.recipient_ref, p.amount_minor, p.currency,
		       p.idempotency_key, p.external_transfer_id::text, p.created_at, et.status
		FROM payouts p JOIN external_transfers et ON et.id = p.external_transfer_id
		WHERE p.merchant_id = $1 AND p.idempotency_key = $2
	`, merchantID, idempotencyKey).Scan(
		&p.ID, &p.MerchantID, &p.Mode, &p.AccountID, &p.RailName, &p.RecipientRef, &p.Amount.Minor, &p.Amount.Currency,
		&p.IdempotencyKey, &p.ExternalTransferID, &p.CreatedAt, &externalStatus,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Payout{}, false, nil
		}
		return Payout{}, false, fmt.Errorf("payouts: lookup by idempotency key: %w", err)
	}
	p.Status = statusFromExternalTransfer(externalStatus)
	return p, true, nil
}

func statusFromExternalTransfer(s externalpayments.Status) Status {
	switch s {
	case externalpayments.StatusCompleted:
		return StatusSucceeded
	case externalpayments.StatusFailed:
		return StatusFailed
	default:
		return StatusPending
	}
}

func resolveSettlementAccount(ctx context.Context, pool *pgxpool.Pool, merchantID, currency string) (string, error) {
	var accountID string
	err := pool.QueryRow(ctx, `
		SELECT id::text FROM accounts WHERE owner_id = $1 AND currency = $2 AND state = 'open' ORDER BY created_at LIMIT 1
	`, merchantID, currency).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoSettlementAccount
		}
		return "", fmt.Errorf("payouts: resolve settlement account: %w", err)
	}
	return accountID, nil
}
