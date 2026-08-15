package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/risk"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func mustMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

func newMerchantWithFundedAccount(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service, fundMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var merchantID string
	err := pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&merchantID)
	if err != nil {
		t.Fatalf("insert test merchant: %v", err)
	}
	acc, err := ledgerSvc.OpenAccount(ctx, merchantID, ledger.AccountTypeCurrent, "NGN", 0)
	if err != nil {
		t.Fatalf("open settlement account: %v", err)
	}
	if fundMinor > 0 {
		if _, err := ledgerSvc.Credit(ctx, acc.ID, mustMoney(t, fundMinor), testsupport.RandomKey()); err != nil {
			t.Fatalf("fund account: %v", err)
		}
	}
	return merchantID
}

func newServices(pool *pgxpool.Pool) (*ledger.Service, *externalpayments.Service, *payouts.Service) {
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(pool, ledgerSvc, registry, risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), nil)
	return ledgerSvc, externalSvc, payouts.NewService(pool, externalSvc, registry)
}

func TestPayoutSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, externalSvc, payoutsSvc := newServices(pool)
	ctx := context.Background()

	merchantID := newMerchantWithFundedAccount(t, pool, ledgerSvc, 100_000_00)

	p, err := payoutsSvc.Create(ctx, merchantID, apikeys.ModeLive, "instant", "recipient-acct-1", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if p.Status != payouts.StatusPending {
		t.Fatalf("status = %s, want pending", p.Status)
	}

	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	got, err := payoutsSvc.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != payouts.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, p.AccountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 90_000_00 {
		t.Fatalf("merchant balance = %d, want 9000000", bal.Available.Minor)
	}
}

func TestPayoutUnknownRailRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, _, payoutsSvc := newServices(pool)
	ctx := context.Background()

	merchantID := newMerchantWithFundedAccount(t, pool, ledgerSvc, 100_000_00)

	_, err := payoutsSvc.Create(ctx, merchantID, apikeys.ModeLive, "not-a-real-rail", "recipient-acct-1", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if !errors.Is(err, payouts.ErrUnknownRail) {
		t.Fatalf("create payout with unknown rail: got %v, want ErrUnknownRail", err)
	}
}

func TestPayoutListReturnsMerchantPayouts(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, _, payoutsSvc := newServices(pool)
	ctx := context.Background()

	merchantID := newMerchantWithFundedAccount(t, pool, ledgerSvc, 100_000_00)

	if _, err := payoutsSvc.Create(ctx, merchantID, apikeys.ModeLive, "instant", "r1", mustMoney(t, 1_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("create payout 1: %v", err)
	}
	if _, err := payoutsSvc.Create(ctx, merchantID, apikeys.ModeLive, "instant", "r2", mustMoney(t, 1_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("create payout 2: %v", err)
	}

	list, err := payoutsSvc.List(ctx, merchantID, apikeys.ModeLive, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("payout count = %d, want 2", len(list))
	}
}
