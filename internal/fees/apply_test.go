package fees_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/fees"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/risk"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
)

func mustMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

func newFundedMerchantAccount(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service, merchantID string, fundMinor int64) string {
	t.Helper()
	ctx := context.Background()
	acc, err := ledgerSvc.OpenAccount(ctx, merchantID, ledger.AccountTypeCurrent, "NGN", 0)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if fundMinor > 0 {
		if _, err := ledgerSvc.Credit(ctx, acc.ID, mustMoney(t, fundMinor), testsupport.RandomKey()); err != nil {
			t.Fatalf("fund account: %v", err)
		}
	}
	return acc.ID
}

func TestApplyFeesToCompletedChargePostsExactlyOnce(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()

	ledgerSvc := ledger.NewService(pool)
	tokensSvc := tokens.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), nil)
	chargesSvc := charges.NewService(pool, tokensSvc, externalSvc)
	feesSvc := fees.NewService(pool, ledgerSvc, nil)

	merchantID := newMerchant(t, pool)
	accountID := newFundedMerchantAccount(t, pool, ledgerSvc, merchantID, 0)

	tok, err := tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	ch, err := chargesSvc.Create(ctx, merchantID, apikeys.ModeSandbox, tok.ID, mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create charge: %v", err)
	}
	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	if err := feesSvc.ApplyFees(ctx); err != nil {
		t.Fatalf("apply fees: %v", err)
	}
	// Second call must be a no-op (fee_charges dedup guard).
	if err := feesSvc.ApplyFees(ctx); err != nil {
		t.Fatalf("apply fees again: %v", err)
	}

	var feeMinor, taxMinor, totalMinor int64
	var ledgerTxnID *string
	if err := pool.QueryRow(ctx, `
		SELECT fee_minor, tax_minor, total_minor, ledger_transaction_id::text FROM fee_charges WHERE source_type = 'charge' AND source_id = $1
	`, ch.ID).Scan(&feeMinor, &taxMinor, &totalMinor, &ledgerTxnID); err != nil {
		t.Fatalf("query fee_charges: %v", err)
	}
	if totalMinor <= 0 {
		t.Fatalf("total fee = %d, want > 0", totalMinor)
	}
	if ledgerTxnID == nil {
		t.Fatal("expected a ledger transaction to have posted the fee")
	}

	bal, err := ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	wantAvailable := int64(1_000_00) - totalMinor
	if bal.Available.Minor != wantAvailable {
		t.Fatalf("merchant available = %d, want %d (charge amount minus fee, applied exactly once)", bal.Available.Minor, wantAvailable)
	}

	platformBal, err := ledgerSvc.GetBalance(ctx, fees.PlatformFeesAccountNGN)
	if err != nil {
		t.Fatalf("get platform balance: %v", err)
	}
	if platformBal.Ledger.Minor < totalMinor {
		t.Fatalf("platform ledger balance = %d, want at least %d", platformBal.Ledger.Minor, totalMinor)
	}
}

func TestApplyFeesToCompletedPayout(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()

	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(pool, ledgerSvc, registry, risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), nil)
	payoutsSvc := payouts.NewService(pool, externalSvc, registry)
	feesSvc := fees.NewService(pool, ledgerSvc, nil)

	merchantID := newMerchant(t, pool)
	accountID := newFundedMerchantAccount(t, pool, ledgerSvc, merchantID, 100_000_00)

	p, err := payoutsSvc.Create(ctx, merchantID, apikeys.ModeLive, "instant", "recipient-1", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	if err := feesSvc.ApplyFees(ctx); err != nil {
		t.Fatalf("apply fees: %v", err)
	}

	var totalMinor int64
	if err := pool.QueryRow(ctx, `SELECT total_minor FROM fee_charges WHERE source_type = 'payout' AND source_id = $1`, p.ID).Scan(&totalMinor); err != nil {
		t.Fatalf("query fee_charges: %v", err)
	}
	if totalMinor <= 0 {
		t.Fatalf("total fee = %d, want > 0 (default payout fee)", totalMinor)
	}

	bal, err := ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	wantAvailable := int64(100_000_00) - 10_000_00 - totalMinor
	if bal.Available.Minor != wantAvailable {
		t.Fatalf("merchant available = %d, want %d", bal.Available.Minor, wantAvailable)
	}
}
