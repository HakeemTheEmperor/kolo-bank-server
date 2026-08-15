// Package fees implements rule-based fee resolution and posting
// (docs/banking-backend-spec.md §4.1): fees vary by merchant, rail, size,
// and currency, and are computed and posted atomically once the
// underlying charge or payout completes. Reuses ledger.Service.Transfer
// (Phase 1) rather than inventing new ledger mechanics — a fee is just a
// transfer from the merchant's settlement account to the platform's fees
// account.
package fees

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlatformFeesAccountNGN is the platform's fee-revenue account, seeded in
// db/migrations/00029_seed_platform_fees_account.sql — same pattern as
// ledger.SystemAccountNGN.
const PlatformFeesAccountNGN = "00000000-0000-0000-0000-000000000003"

type Flow string

const (
	FlowCharge Flow = "charge"
	FlowPayout Flow = "payout"
)

// Breakdown is a resolved fee ready to be posted.
type Breakdown struct {
	RuleID     string
	FeeMinor   int64
	TaxMinor   int64
	TotalMinor int64
}

// Resolve picks the most specific matching fee_rules row (merchant-specific
// over the default NULL rule, exact rail over the '*' wildcard) and
// computes the fee: percent*amount + fixed, capped if a cap is set, plus
// tax on the fee.
func Resolve(ctx context.Context, pool *pgxpool.Pool, merchantID string, flow Flow, rail, currency string, amountMinor int64) (Breakdown, error) {
	var (
		ruleID        string
		percentBps    int64
		fixedMinor    int64
		capMinor      *int64
		taxPercentBps int64
	)
	err := pool.QueryRow(ctx, `
		SELECT id::text, percent_bps, fixed_minor, cap_minor, tax_percent_bps
		FROM fee_rules
		WHERE active
		  AND flow = $1
		  AND currency = $2
		  AND (rail = $3 OR rail = '*')
		  AND min_amount_minor <= $4
		  AND (max_amount_minor IS NULL OR max_amount_minor >= $4)
		  AND (merchant_id = $5 OR merchant_id IS NULL)
		ORDER BY (merchant_id IS NOT NULL) DESC, (rail <> '*') DESC
		LIMIT 1
	`, flow, currency, rail, amountMinor, merchantID).Scan(&ruleID, &percentBps, &fixedMinor, &capMinor, &taxPercentBps)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No applicable rule: charge nothing rather than fail the
			// underlying transfer over a pricing-config gap.
			return Breakdown{}, nil
		}
		return Breakdown{}, fmt.Errorf("fees: resolve rule: %w", err)
	}

	fee := amountMinor*percentBps/10000 + fixedMinor
	if capMinor != nil && fee > *capMinor {
		fee = *capMinor
	}
	tax := fee * taxPercentBps / 10000

	return Breakdown{RuleID: ruleID, FeeMinor: fee, TaxMinor: tax, TotalMinor: fee + tax}, nil
}
