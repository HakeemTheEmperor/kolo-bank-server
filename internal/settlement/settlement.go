// Package settlement implements the settlement engine with rolling
// reserves (docs/banking-backend-spec.md §4.2): a merchant's collections
// aggregate per cycle, net of fees, with a percentage held back against
// future chargebacks and released after a hold period. Pays out through
// the exact same payouts.Service.Create (Phase 5) an API-initiated payout
// would use — settlement is not a separate money-movement mechanism.
package settlement

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/payouts"
)

type Interval string

const (
	IntervalDaily  Interval = "daily"
	IntervalWeekly Interval = "weekly"
)

type Config struct {
	MerchantID        string
	Currency          string
	ReservePercentBps int
	ReserveHoldDays   int
	RecipientRef      string
	Rail              string
	CycleInterval     Interval
	NextCycleAt       time.Time
}

type Cycle struct {
	ID               string
	MerchantID       string
	Currency         string
	GrossMinor       int64
	FeesMinor        int64
	ReserveMinor     int64
	NetMinor         int64
	PayoutID         *string
	ReserveReleaseAt time.Time
	ReserveReleased  bool
}

type Service struct {
	pool       *pgxpool.Pool
	payoutsSvc *payouts.Service
	logger     *slog.Logger
}

func NewService(pool *pgxpool.Pool, payoutsSvc *payouts.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, payoutsSvc: payoutsSvc, logger: logger}
}

// CreateConfig sets up (or replaces) a merchant's settlement configuration.
func (s *Service) CreateConfig(ctx context.Context, merchantID, currency string, reservePercentBps, reserveHoldDays int, recipientRef, rail string, cycleInterval Interval, firstCycleAt time.Time) (Config, error) {
	var c Config
	err := s.pool.QueryRow(ctx, `
		INSERT INTO merchant_settlement_configs (merchant_id, currency, reserve_percent_bps, reserve_hold_days, recipient_ref, rail, cycle_interval, next_cycle_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (merchant_id) DO UPDATE SET
			currency = EXCLUDED.currency, reserve_percent_bps = EXCLUDED.reserve_percent_bps, reserve_hold_days = EXCLUDED.reserve_hold_days,
			recipient_ref = EXCLUDED.recipient_ref, rail = EXCLUDED.rail, cycle_interval = EXCLUDED.cycle_interval, next_cycle_at = EXCLUDED.next_cycle_at
		RETURNING merchant_id::text, currency, reserve_percent_bps, reserve_hold_days, recipient_ref, rail, cycle_interval, next_cycle_at
	`, merchantID, currency, reservePercentBps, reserveHoldDays, recipientRef, rail, cycleInterval, firstCycleAt).Scan(
		&c.MerchantID, &c.Currency, &c.ReservePercentBps, &c.ReserveHoldDays, &c.RecipientRef, &c.Rail, &c.CycleInterval, &c.NextCycleAt,
	)
	if err != nil {
		return Config{}, fmt.Errorf("settlement: create config: %w", err)
	}
	return c, nil
}

func (s *Service) GetCycle(ctx context.Context, id string) (Cycle, error) {
	var c Cycle
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, currency, gross_minor, fees_minor, reserve_minor, net_minor, payout_id::text, reserve_release_at, reserve_released
		FROM settlement_cycles WHERE id = $1
	`, id).Scan(&c.ID, &c.MerchantID, &c.Currency, &c.GrossMinor, &c.FeesMinor, &c.ReserveMinor, &c.NetMinor, &c.PayoutID, &c.ReserveReleaseAt, &c.ReserveReleased)
	if err != nil {
		return Cycle{}, fmt.Errorf("settlement: get cycle: %w", err)
	}
	return c, nil
}

func advance(from time.Time, interval Interval) time.Time {
	if interval == IntervalWeekly {
		return from.AddDate(0, 0, 7)
	}
	return from.AddDate(0, 0, 1)
}
