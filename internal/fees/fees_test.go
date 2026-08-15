package fees_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/fees"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func newMerchant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test merchant: %v", err)
	}
	return id
}

func expectedFee(amountMinor, percentBps, fixedMinor int64, capMinor *int64, taxPercentBps int64) (feeMinor, taxMinor, totalMinor int64) {
	fee := amountMinor*percentBps/10000 + fixedMinor
	if capMinor != nil && fee > *capMinor {
		fee = *capMinor
	}
	tax := fee * taxPercentBps / 10000
	return fee, tax, fee + tax
}

func TestResolveDefaultChargeRule(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	merchantID := newMerchant(t, pool)

	const amount int64 = 1_000_00
	b, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "instant", "NGN", amount)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	cap := int64(2000_00)
	wantFee, wantTax, wantTotal := expectedFee(amount, 150, 100_00, &cap, 750)
	if b.FeeMinor != wantFee || b.TaxMinor != wantTax || b.TotalMinor != wantTotal {
		t.Fatalf("breakdown = %+v, want fee=%d tax=%d total=%d", b, wantFee, wantTax, wantTotal)
	}
}

func TestResolveMerchantOverrideBeatsDefault(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	merchantID := newMerchant(t, pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO fee_rules (merchant_id, flow, rail, currency, percent_bps, fixed_minor, tax_percent_bps)
		VALUES ($1, 'charge', '*', 'NGN', 50, 0, 0)
	`, merchantID); err != nil {
		t.Fatalf("insert override rule: %v", err)
	}

	const amount int64 = 1_000_00
	b, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "instant", "NGN", amount)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantFee := amount * 50 / 10000
	if b.FeeMinor != wantFee || b.TaxMinor != 0 {
		t.Fatalf("breakdown = %+v, want fee=%d tax=0 (merchant override, not default)", b, wantFee)
	}
}

func TestResolveExactRailBeatsWildcard(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	merchantID := newMerchant(t, pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO fee_rules (flow, rail, currency, percent_bps, fixed_minor, tax_percent_bps)
		VALUES ('charge', 'wire', 'NGN', 999, 0, 0)
	`); err != nil {
		t.Fatalf("insert rail-specific rule: %v", err)
	}

	const amount int64 = 1_000_00
	b, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "wire", "NGN", amount)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantFee := amount * 999 / 10000
	if b.FeeMinor != wantFee {
		t.Fatalf("fee = %d, want %d (exact rail rule should beat the '*' default)", b.FeeMinor, wantFee)
	}

	// A different rail on the same merchant must still fall back to the
	// '*' default, not the wire-specific rule.
	other, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "instant", "NGN", amount)
	if err != nil {
		t.Fatalf("resolve other rail: %v", err)
	}
	if other.FeeMinor == wantFee {
		t.Fatal("expected a different rail to not pick up the wire-specific rule")
	}
}

func TestResolveCapEnforced(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	merchantID := newMerchant(t, pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO fee_rules (merchant_id, flow, rail, currency, percent_bps, fixed_minor, cap_minor, tax_percent_bps)
		VALUES ($1, 'charge', '*', 'NGN', 5000, 0, 100_00, 0)
	`, merchantID); err != nil {
		t.Fatalf("insert capped rule: %v", err)
	}

	b, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "instant", "NGN", 10_000_00)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.FeeMinor != 100_00 {
		t.Fatalf("fee = %d, want capped at 10000", b.FeeMinor)
	}
}

func TestResolveNoMatchingRuleReturnsZero(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	merchantID := newMerchant(t, pool)

	b, err := fees.Resolve(ctx, pool, merchantID, fees.FlowCharge, "instant", "USD", 1_000_00)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.TotalMinor != 0 {
		t.Fatalf("total = %d, want 0 (no USD rule configured)", b.TotalMinor)
	}
}
