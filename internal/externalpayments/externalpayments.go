// Package externalpayments routes money across the bank's boundary through
// a simulated external rail (docs/banking-backend-spec.md §3.4). It adds no
// new ledger primitives: outbound transfers reuse holds
// (ledger.Service.PlaceHold/CaptureHold/ReleaseHold) exactly as designed —
// reserve now, settle or release once the rail responds — and inbound
// transfers reuse ledger.Service.Credit. The claim -> external-call ->
// finalize shape, and the in-flight resolver that retries a stuck claim
// with the same deterministic key, mirror internal/scheduler (Phase 3)
// exactly; only the "external call" step (a rail, not a due-time check)
// differs.
package externalpayments

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// stuckTimeout mirrors scheduler.stuckTimeout (internal/scheduler/scheduler.go).
const stuckTimeout = 2 * time.Minute

type ExternalTransfer struct {
	ID                  string
	Direction           rails.Direction
	AccountID           string
	RailName            string
	CounterpartyRef     string
	Amount              ledger.Money
	IdempotencyKey      string
	HoldID              *string
	BatchID             *string
	Status              Status
	RailReference       *string
	LedgerTransactionID *string
	Error               *string
	CreatedAt           time.Time
}

type OutboundItem struct {
	AccountID       string
	RailName        string
	CounterpartyRef string
	Amount          ledger.Money
	IdempotencyKey  string
}

type Service struct {
	pool      *pgxpool.Pool
	ledgerSvc *ledger.Service
	registry  *rails.Registry
	logger    *slog.Logger
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service, registry *rails.Registry, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, ledgerSvc: ledgerSvc, registry: registry, logger: logger}
}

// SendOutbound reserves amount via a hold and queues it for processing by
// ProcessPending. Idempotent: retrying with the same idempotencyKey
// returns the original row rather than placing a second hold.
func (s *Service) SendOutbound(ctx context.Context, accountID, railName, counterpartyRef string, amount ledger.Money, idempotencyKey string) (ExternalTransfer, error) {
	return s.send(ctx, rails.DirectionOutbound, accountID, railName, counterpartyRef, amount, idempotencyKey, nil)
}

// SendInbound queues an inbound settlement for processing; unlike
// SendOutbound there is nothing to reserve.
func (s *Service) SendInbound(ctx context.Context, accountID, railName, counterpartyRef string, amount ledger.Money, idempotencyKey string) (ExternalTransfer, error) {
	return s.send(ctx, rails.DirectionInbound, accountID, railName, counterpartyRef, amount, idempotencyKey, nil)
}

// CreateBatch submits N outbound transfers under one shared batch id —
// "bulk/batch payouts routed through the rail interface"
// (docs/banking-backend-spec.md §3.4) — without needing a separate batch
// table; each item still processes (and can fail) independently.
func (s *Service) CreateBatch(ctx context.Context, items []OutboundItem) ([]ExternalTransfer, error) {
	batchID := generateID()
	out := make([]ExternalTransfer, 0, len(items))
	for _, item := range items {
		et, err := s.send(ctx, rails.DirectionOutbound, item.AccountID, item.RailName, item.CounterpartyRef, item.Amount, item.IdempotencyKey, &batchID)
		if err != nil {
			return out, fmt.Errorf("externalpayments: batch item %s: %w", item.IdempotencyKey, err)
		}
		out = append(out, et)
	}
	return out, nil
}

func (s *Service) send(ctx context.Context, direction rails.Direction, accountID, railName, counterpartyRef string, amount ledger.Money, idempotencyKey string, batchID *string) (ExternalTransfer, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, idempotencyKey); err != nil {
		return ExternalTransfer{}, err
	} else if ok {
		return existing, nil
	}

	var holdID *string
	if direction == rails.DirectionOutbound {
		hold, err := s.ledgerSvc.PlaceHold(ctx, accountID, amount)
		if err != nil {
			return ExternalTransfer{}, fmt.Errorf("externalpayments: place hold: %w", err)
		}
		holdID = &hold.ID
	}

	var et ExternalTransfer
	err := s.pool.QueryRow(ctx, `
		INSERT INTO external_transfers (direction, account_id, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id, batch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id::text, direction, account_id::text, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id::text, batch_id::text, status, created_at
	`, direction, accountID, railName, counterpartyRef, amount.Minor, amount.Currency, idempotencyKey, holdID, batchID).Scan(
		&et.ID, &et.Direction, &et.AccountID, &et.RailName, &et.CounterpartyRef, &et.Amount.Minor, &et.Amount.Currency,
		&et.IdempotencyKey, &et.HoldID, &et.BatchID, &et.Status, &et.CreatedAt,
	)
	if err != nil {
		return ExternalTransfer{}, fmt.Errorf("externalpayments: create: %w", err)
	}
	return et, nil
}

func (s *Service) getByIdempotencyKey(ctx context.Context, idempotencyKey string) (ExternalTransfer, bool, error) {
	var et ExternalTransfer
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, direction, account_id::text, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id::text, batch_id::text, status, created_at
		FROM external_transfers WHERE idempotency_key = $1
	`, idempotencyKey).Scan(
		&et.ID, &et.Direction, &et.AccountID, &et.RailName, &et.CounterpartyRef, &et.Amount.Minor, &et.Amount.Currency,
		&et.IdempotencyKey, &et.HoldID, &et.BatchID, &et.Status, &et.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExternalTransfer{}, false, nil
		}
		return ExternalTransfer{}, false, fmt.Errorf("externalpayments: lookup by idempotency key: %w", err)
	}
	return et, true, nil
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
