package fees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

const applyBatchSize = 50

type Service struct {
	pool      *pgxpool.Pool
	ledgerSvc *ledger.Service
	logger    *slog.Logger
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, ledgerSvc: ledgerSvc, logger: logger}
}

type feeableRow struct {
	SourceID   string
	MerchantID string
	AccountID  string
	AmountMinor int64
	Currency   string
	RailName   string
}

// ApplyFees resolves and posts the fee for every completed charge/payout
// that hasn't been fee-processed yet (no existing fee_charges row is the
// dedup guard — see 00031_create_fee_charges.sql). Safe to call
// repeatedly: once a fee_charges row exists for a source, it's skipped.
func (s *Service) ApplyFees(ctx context.Context) error {
	if err := s.applyFor(ctx, "charge", FlowCharge); err != nil {
		return fmt.Errorf("fees: apply to charges: %w", err)
	}
	if err := s.applyFor(ctx, "payout", FlowPayout); err != nil {
		return fmt.Errorf("fees: apply to payouts: %w", err)
	}
	return nil
}

func (s *Service) applyFor(ctx context.Context, sourceType string, flow Flow) error {
	table := "charges"
	if sourceType == "payout" {
		table = "payouts"
	}

	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.merchant_id::text, c.account_id::text, c.amount_minor, c.currency, et.rail_name
		FROM `+table+` c
		JOIN external_transfers et ON et.id = c.external_transfer_id
		WHERE et.status = 'completed'
		  AND NOT EXISTS (SELECT 1 FROM fee_charges fc WHERE fc.source_type = $1 AND fc.source_id = c.id)
		LIMIT $2
	`, sourceType, applyBatchSize)
	if err != nil {
		return err
	}

	var items []feeableRow
	for rows.Next() {
		var r feeableRow
		if err := rows.Scan(&r.SourceID, &r.MerchantID, &r.AccountID, &r.AmountMinor, &r.Currency, &r.RailName); err != nil {
			rows.Close()
			return fmt.Errorf("scan feeable row: %w", err)
		}
		items = append(items, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range items {
		if err := s.applyOne(ctx, sourceType, flow, item); err != nil {
			s.logger.ErrorContext(ctx, "fees: apply failed", slog.String("source_type", sourceType), slog.String("source_id", item.SourceID), slog.Any("error", err))
		}
	}
	return nil
}

func (s *Service) applyOne(ctx context.Context, sourceType string, flow Flow, item feeableRow) error {
	breakdown, err := Resolve(ctx, s.pool, item.MerchantID, flow, item.RailName, item.Currency, item.AmountMinor)
	if err != nil {
		return err
	}

	var ledgerTxnID *string
	if breakdown.TotalMinor > 0 {
		amount, err := ledger.NewMoney(breakdown.TotalMinor, item.Currency)
		if err != nil {
			return err
		}
		idemKey := "fee:" + sourceType + ":" + item.SourceID
		txn, err := s.ledgerSvc.Transfer(ctx, item.AccountID, PlatformFeesAccountNGN, amount, idemKey)
		if err != nil {
			if errors.Is(err, ledger.ErrInsufficientBalance) {
				// Leave unprocessed; retried on the next tick once the
				// merchant's balance covers the fee.
				return fmt.Errorf("insufficient balance for fee, will retry: %w", err)
			}
			return err
		}
		ledgerTxnID = &txn.ID
	}

	var ruleID *string
	if breakdown.RuleID != "" {
		ruleID = &breakdown.RuleID
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO fee_charges (source_type, source_id, fee_rule_id, fee_minor, tax_minor, total_minor, currency, ledger_transaction_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, sourceType, item.SourceID, ruleID, breakdown.FeeMinor, breakdown.TaxMinor, breakdown.TotalMinor, item.Currency, ledgerTxnID)
	if err != nil {
		return fmt.Errorf("record fee charge: %w", err)
	}
	return nil
}
