// Package reconciliation implements automated multi-way reconciliation
// with break detection (docs/banking-backend-spec.md §4.3): the internal
// ledger and a partner-bank/rail statement will disagree because of
// timing, dropped webhooks, and partial failures. This is simulated by
// mirroring external_transfers (internal/externalpayments) into
// "statement lines" independent of our own processing state — generated
// as soon as a transfer exists, not after it settles, so a line can
// genuinely still be waiting on our side to catch up (the benign-timing
// case) rather than always trivially matching by construction.
package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// breakMarker in a transfer's counterparty_ref deterministically produces
// a mismatched statement report, the same controllable-simulation
// convention as rails' RAILFAIL, kyc's KYCFAIL, and tokens' reserved
// decline numbers — giving tests a reliable way to seed a genuine break.
const breakMarker = "RECONBREAK"

// breakDelta is how far off a seeded-mismatch statement line reports,
// relative to the true transfer amount.
const breakDelta = 100_00

const batchSize = 100

type Service struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, logger: logger}
}

// GenerateStatementLines mirrors any external_transfers row without a
// statement line yet into one, simulating a partner report that can
// arrive before our own side has finished processing it.
func (s *Service) GenerateStatementLines(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, amount_minor, currency, counterparty_ref
		FROM external_transfers et
		WHERE NOT EXISTS (SELECT 1 FROM reconciliation_statement_lines l WHERE l.external_transfer_id = et.id)
		LIMIT $1
	`, batchSize)
	if err != nil {
		return fmt.Errorf("reconciliation: find new transfers: %w", err)
	}

	type transfer struct {
		ID              string
		AmountMinor     int64
		Currency        string
		CounterpartyRef string
	}
	var transfers []transfer
	for rows.Next() {
		var t transfer
		if err := rows.Scan(&t.ID, &t.AmountMinor, &t.Currency, &t.CounterpartyRef); err != nil {
			rows.Close()
			return fmt.Errorf("scan transfer: %w", err)
		}
		transfers = append(transfers, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, t := range transfers {
		reported := t.AmountMinor
		if strings.Contains(t.CounterpartyRef, breakMarker) {
			reported += breakDelta
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO reconciliation_statement_lines (external_transfer_id, reported_amount_minor, currency)
			VALUES ($1, $2, $3)
			ON CONFLICT (external_transfer_id) DO NOTHING
		`, t.ID, reported, t.Currency); err != nil {
			s.logger.ErrorContext(ctx, "reconciliation: insert statement line failed", slog.String("external_transfer_id", t.ID), slog.Any("error", err))
		}
	}
	return nil
}
