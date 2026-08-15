package settlement

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

// RunCycles processes every merchant settlement config whose next_cycle_at
// has arrived: aggregates eligible charges into a cycle and pays out the
// net amount.
func (s *Service) RunCycles(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT merchant_id::text, currency, reserve_percent_bps, reserve_hold_days, recipient_ref, rail, cycle_interval, next_cycle_at
		FROM merchant_settlement_configs WHERE next_cycle_at <= now()
	`)
	if err != nil {
		return fmt.Errorf("settlement: find due configs: %w", err)
	}

	var due []Config
	for rows.Next() {
		var c Config
		if err := rows.Scan(&c.MerchantID, &c.Currency, &c.ReservePercentBps, &c.ReserveHoldDays, &c.RecipientRef, &c.Rail, &c.CycleInterval, &c.NextCycleAt); err != nil {
			rows.Close()
			return fmt.Errorf("settlement: scan config: %w", err)
		}
		due = append(due, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, cfg := range due {
		if err := s.runCycleFor(ctx, cfg); err != nil {
			s.logger.ErrorContext(ctx, "settlement: run cycle failed", slog.String("merchant_id", cfg.MerchantID), slog.Any("error", err))
		}
	}
	return nil
}

type eligibleCharge struct {
	ID          string
	AmountMinor int64
	FeeMinor    int64
}

func (s *Service) runCycleFor(ctx context.Context, cfg Config) error {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.amount_minor, fc.total_minor
		FROM charges c
		JOIN external_transfers et ON et.id = c.external_transfer_id
		JOIN ledger_transactions lt ON lt.id = et.ledger_transaction_id
		JOIN fee_charges fc ON fc.source_type = 'charge' AND fc.source_id = c.id
		LEFT JOIN settlement_cycle_items sci ON sci.charge_id = c.id
		WHERE c.merchant_id = $1 AND c.currency = $2 AND c.mode = 'live'
		  AND et.status = 'completed' AND lt.state = 'posted'
		  AND sci.charge_id IS NULL
	`, cfg.MerchantID, cfg.Currency)
	if err != nil {
		return fmt.Errorf("find eligible charges: %w", err)
	}

	var charges []eligibleCharge
	for rows.Next() {
		var c eligibleCharge
		if err := rows.Scan(&c.ID, &c.AmountMinor, &c.FeeMinor); err != nil {
			rows.Close()
			return fmt.Errorf("scan eligible charge: %w", err)
		}
		charges = append(charges, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.advanceSchedule(ctx, cfg); err != nil {
		return err
	}

	if len(charges) == 0 {
		return nil // nothing to settle this cycle
	}

	var gross, feesTotal int64
	for _, c := range charges {
		gross += c.AmountMinor
		feesTotal += c.FeeMinor
	}
	net := gross - feesTotal
	reserve := net * int64(cfg.ReservePercentBps) / 10000
	payout := net - reserve

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cycleID string
	err = tx.QueryRow(ctx, `
		INSERT INTO settlement_cycles (merchant_id, currency, gross_minor, fees_minor, reserve_minor, net_minor, reserve_release_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(days => $7))
		RETURNING id::text
	`, cfg.MerchantID, cfg.Currency, gross, feesTotal, reserve, net, cfg.ReserveHoldDays).Scan(&cycleID)
	if err != nil {
		return fmt.Errorf("insert cycle: %w", err)
	}

	for _, c := range charges {
		if _, err := tx.Exec(ctx, `INSERT INTO settlement_cycle_items (settlement_cycle_id, charge_id) VALUES ($1, $2)`, cycleID, c.ID); err != nil {
			return fmt.Errorf("insert cycle item: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if payout > 0 {
		amount, err := ledger.NewMoney(payout, cfg.Currency)
		if err != nil {
			return err
		}
		p, err := s.payoutsSvc.Create(ctx, cfg.MerchantID, apikeys.ModeLive, cfg.Rail, cfg.RecipientRef, amount, "settlement:"+cycleID)
		if err != nil {
			return fmt.Errorf("create settlement payout: %w", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE settlement_cycles SET payout_id = $2 WHERE id = $1`, cycleID, p.ID); err != nil {
			return fmt.Errorf("link settlement payout: %w", err)
		}
	}

	return nil
}

func (s *Service) advanceSchedule(ctx context.Context, cfg Config) error {
	next := advance(cfg.NextCycleAt, cfg.CycleInterval)
	_, err := s.pool.Exec(ctx, `UPDATE merchant_settlement_configs SET next_cycle_at = $2 WHERE merchant_id = $1`, cfg.MerchantID, next)
	if err != nil {
		return fmt.Errorf("advance settlement schedule: %w", err)
	}
	return nil
}

// ReleaseMaturedReserves pays out the held-back reserve for every cycle
// whose hold period has passed.
func (s *Service) ReleaseMaturedReserves(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT sc.id::text, sc.merchant_id::text, sc.currency, sc.reserve_minor, msc.rail, msc.recipient_ref
		FROM settlement_cycles sc
		JOIN merchant_settlement_configs msc ON msc.merchant_id = sc.merchant_id
		WHERE NOT sc.reserve_released AND sc.reserve_minor > 0 AND sc.reserve_release_at <= now()
	`)
	if err != nil {
		return fmt.Errorf("settlement: find matured reserves: %w", err)
	}

	type matured struct {
		CycleID, MerchantID, Currency, Rail, RecipientRef string
		ReserveMinor                                      int64
	}
	var items []matured
	for rows.Next() {
		var m matured
		if err := rows.Scan(&m.CycleID, &m.MerchantID, &m.Currency, &m.ReserveMinor, &m.Rail, &m.RecipientRef); err != nil {
			rows.Close()
			return fmt.Errorf("scan matured reserve: %w", err)
		}
		items = append(items, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range items {
		amount, err := ledger.NewMoney(m.ReserveMinor, m.Currency)
		if err != nil {
			s.logger.ErrorContext(ctx, "settlement: release reserve failed", slog.String("cycle_id", m.CycleID), slog.Any("error", err))
			continue
		}
		p, err := s.payoutsSvc.Create(ctx, m.MerchantID, apikeys.ModeLive, m.Rail, m.RecipientRef, amount, "settlement-reserve:"+m.CycleID)
		if err != nil {
			s.logger.ErrorContext(ctx, "settlement: release reserve payout failed", slog.String("cycle_id", m.CycleID), slog.Any("error", err))
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE settlement_cycles SET reserve_released = true, reserve_payout_id = $2 WHERE id = $1
		`, m.CycleID, p.ID); err != nil {
			s.logger.ErrorContext(ctx, "settlement: mark reserve released failed", slog.String("cycle_id", m.CycleID), slog.Any("error", err))
		}
	}
	return nil
}
