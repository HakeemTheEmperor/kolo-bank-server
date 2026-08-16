// Package charges implements collections — charging a customer's tokenized
// card (docs/banking-backend-spec.md §3.6). A charge is a thin record over
// an inbound external transfer (internal/externalpayments): its status is
// read by joining to external_transfers, never duplicated, the same
// decision Phase 4's bill_payments already made. Execution (the actual
// rail call) is entirely handled by externalpayments' existing
// ProcessPending/ResolveStuck tickers — charges needs no pipeline of its
// own.
package charges

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
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
)

// ErrNoSettlementAccount is returned when the merchant has no open account
// to receive funds into.
var ErrNoSettlementAccount = errors.New("charges: merchant has no open settlement account")

// cardRail is the simulated rail charges route through
// (internal/rails.Registry).
const cardRail = "card"

// railFailMarker mirrors internal/rails' failMarker convention.
const railFailMarker = "RAILFAIL"

type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Charge struct {
	ID                 string
	MerchantID         string
	Mode               apikeys.Mode
	TokenID            string
	AccountID          string
	Amount             ledger.Money
	IdempotencyKey     string
	ExternalTransferID string
	Status             Status
	CreatedAt          time.Time
}

type Service struct {
	pool          *pgxpool.Pool
	tokensSvc     *tokens.Service
	externalSvc   *externalpayments.Service
	resilienceSvc *resilience.Service
}

func NewService(pool *pgxpool.Pool, tokensSvc *tokens.Service, externalSvc *externalpayments.Service, resilienceSvc *resilience.Service) *Service {
	return &Service{pool: pool, tokensSvc: tokensSvc, externalSvc: externalSvc, resilienceSvc: resilienceSvc}
}

// Create charges tokenID for amount into the merchant's settlement
// account. Idempotent per (merchant, idempotencyKey).
func (s *Service) Create(ctx context.Context, merchantID string, mode apikeys.Mode, tokenID string, amount ledger.Money, idempotencyKey string) (Charge, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, merchantID, idempotencyKey); err != nil {
		return Charge{}, err
	} else if ok {
		return existing, nil
	}

	if err := s.resilienceSvc.Check(ctx, resilience.Feature("charge"), resilience.Merchant(merchantID)); err != nil {
		return Charge{}, err
	}

	tok, err := s.tokensSvc.Get(ctx, tokenID)
	if err != nil {
		return Charge{}, fmt.Errorf("charges: look up token: %w", err)
	}

	accountID, err := resolveSettlementAccount(ctx, s.pool, merchantID, amount.Currency)
	if err != nil {
		return Charge{}, err
	}

	counterpartyRef := "card:" + tok.ID
	if tok.WillFail {
		counterpartyRef += ":" + railFailMarker
	}

	et, err := s.externalSvc.SendInbound(ctx, accountID, cardRail, counterpartyRef, amount, idempotencyKey)
	if err != nil {
		return Charge{}, fmt.Errorf("charges: send inbound: %w", err)
	}

	var ch Charge
	err = s.pool.QueryRow(ctx, `
		INSERT INTO charges (merchant_id, mode, token_id, account_id, amount_minor, currency, idempotency_key, external_transfer_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (merchant_id, idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING id::text, merchant_id::text, mode, token_id::text, account_id::text, amount_minor, currency, idempotency_key, external_transfer_id::text, created_at
	`, merchantID, mode, tokenID, accountID, amount.Minor, amount.Currency, idempotencyKey, et.ID).Scan(
		&ch.ID, &ch.MerchantID, &ch.Mode, &ch.TokenID, &ch.AccountID, &ch.Amount.Minor, &ch.Amount.Currency,
		&ch.IdempotencyKey, &ch.ExternalTransferID, &ch.CreatedAt,
	)
	if err != nil {
		return Charge{}, fmt.Errorf("charges: record: %w", err)
	}

	ch.Status = statusFromExternalTransfer(externalpayments.StatusPending)
	return ch, nil
}

// Get returns a charge with its current status resolved from the linked
// external transfer.
func (s *Service) Get(ctx context.Context, id string) (Charge, error) {
	var ch Charge
	var externalStatus externalpayments.Status
	err := s.pool.QueryRow(ctx, `
		SELECT c.id::text, c.merchant_id::text, c.mode, c.token_id::text, c.account_id::text, c.amount_minor, c.currency,
		       c.idempotency_key, c.external_transfer_id::text, c.created_at, et.status
		FROM charges c JOIN external_transfers et ON et.id = c.external_transfer_id
		WHERE c.id = $1
	`, id).Scan(
		&ch.ID, &ch.MerchantID, &ch.Mode, &ch.TokenID, &ch.AccountID, &ch.Amount.Minor, &ch.Amount.Currency,
		&ch.IdempotencyKey, &ch.ExternalTransferID, &ch.CreatedAt, &externalStatus,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Charge{}, fmt.Errorf("charges: %s: %w", id, pgx.ErrNoRows)
		}
		return Charge{}, fmt.Errorf("charges: get: %w", err)
	}
	ch.Status = statusFromExternalTransfer(externalStatus)
	return ch, nil
}

// List returns a merchant's charges in a mode, newest first — this list
// (and Get above) is the developer-facing transaction log
// (docs/banking-backend-spec.md §3.6).
func (s *Service) List(ctx context.Context, merchantID string, mode apikeys.Mode, limit int) ([]Charge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.merchant_id::text, c.mode, c.token_id::text, c.account_id::text, c.amount_minor, c.currency,
		       c.idempotency_key, c.external_transfer_id::text, c.created_at, et.status
		FROM charges c JOIN external_transfers et ON et.id = c.external_transfer_id
		WHERE c.merchant_id = $1 AND c.mode = $2
		ORDER BY c.created_at DESC
		LIMIT $3
	`, merchantID, mode, limit)
	if err != nil {
		return nil, fmt.Errorf("charges: list: %w", err)
	}
	defer rows.Close()

	var out []Charge
	for rows.Next() {
		var ch Charge
		var externalStatus externalpayments.Status
		if err := rows.Scan(&ch.ID, &ch.MerchantID, &ch.Mode, &ch.TokenID, &ch.AccountID, &ch.Amount.Minor, &ch.Amount.Currency,
			&ch.IdempotencyKey, &ch.ExternalTransferID, &ch.CreatedAt, &externalStatus); err != nil {
			return nil, fmt.Errorf("charges: scan: %w", err)
		}
		ch.Status = statusFromExternalTransfer(externalStatus)
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (s *Service) getByIdempotencyKey(ctx context.Context, merchantID, idempotencyKey string) (Charge, bool, error) {
	var ch Charge
	var externalStatus externalpayments.Status
	err := s.pool.QueryRow(ctx, `
		SELECT c.id::text, c.merchant_id::text, c.mode, c.token_id::text, c.account_id::text, c.amount_minor, c.currency,
		       c.idempotency_key, c.external_transfer_id::text, c.created_at, et.status
		FROM charges c JOIN external_transfers et ON et.id = c.external_transfer_id
		WHERE c.merchant_id = $1 AND c.idempotency_key = $2
	`, merchantID, idempotencyKey).Scan(
		&ch.ID, &ch.MerchantID, &ch.Mode, &ch.TokenID, &ch.AccountID, &ch.Amount.Minor, &ch.Amount.Currency,
		&ch.IdempotencyKey, &ch.ExternalTransferID, &ch.CreatedAt, &externalStatus,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Charge{}, false, nil
		}
		return Charge{}, false, fmt.Errorf("charges: lookup by idempotency key: %w", err)
	}
	ch.Status = statusFromExternalTransfer(externalStatus)
	return ch, true, nil
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

// resolveSettlementAccount picks the merchant's earliest open account in
// currency — same simplification payments.SendToRecipientEmail already
// documents (internal/payments/payments.go): no "primary account" concept
// yet.
func resolveSettlementAccount(ctx context.Context, pool *pgxpool.Pool, merchantID, currency string) (string, error) {
	var accountID string
	err := pool.QueryRow(ctx, `
		SELECT id::text FROM accounts WHERE owner_id = $1 AND currency = $2 AND state = 'open' ORDER BY created_at LIMIT 1
	`, merchantID, currency).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoSettlementAccount
		}
		return "", fmt.Errorf("charges: resolve settlement account: %w", err)
	}
	return accountID, nil
}
