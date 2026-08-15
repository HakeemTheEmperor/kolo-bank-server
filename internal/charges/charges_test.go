package charges_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
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

func newMerchantWithAccount(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service) string {
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
	if _, err := ledgerSvc.OpenAccount(ctx, merchantID, ledger.AccountTypeCurrent, "NGN", 0); err != nil {
		t.Fatalf("open settlement account: %v", err)
	}
	return merchantID
}

func newServices(pool *pgxpool.Pool) (*ledger.Service, *tokens.Service, *externalpayments.Service, *charges.Service) {
	ledgerSvc := ledger.NewService(pool)
	tokensSvc := tokens.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), nil)
	return ledgerSvc, tokensSvc, externalSvc, charges.NewService(pool, tokensSvc, externalSvc)
}

func TestChargeSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, tokensSvc, externalSvc, chargesSvc := newServices(pool)
	ctx := context.Background()

	merchantID := newMerchantWithAccount(t, pool, ledgerSvc)
	tok, err := tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	ch, err := chargesSvc.Create(ctx, merchantID, apikeys.ModeSandbox, tok.ID, mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create charge: %v", err)
	}
	if ch.Status != charges.StatusPending {
		t.Fatalf("status = %s, want pending", ch.Status)
	}

	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	got, err := chargesSvc.Get(ctx, ch.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != charges.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, ch.AccountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 5_000_00 {
		t.Fatalf("merchant balance = %d, want 500000", bal.Available.Minor)
	}
}

func TestChargeWithDeclineTokenFails(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, tokensSvc, externalSvc, chargesSvc := newServices(pool)
	ctx := context.Background()

	merchantID := newMerchantWithAccount(t, pool, ledgerSvc)
	tok, err := tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4000000000000002", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	ch, err := chargesSvc.Create(ctx, merchantID, apikeys.ModeSandbox, tok.ID, mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create charge: %v", err)
	}

	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	got, err := chargesSvc.Get(ctx, ch.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != charges.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, ch.AccountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 0 {
		t.Fatalf("merchant balance = %d, want 0 (declined charge must not credit)", bal.Available.Minor)
	}
}

func TestChargeWithoutSettlementAccountFails(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	_, tokensSvc, _, chargesSvc := newServices(pool)
	ctx := context.Background()

	var merchantID string
	err := pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'No Account Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&merchantID)
	if err != nil {
		t.Fatalf("insert merchant: %v", err)
	}

	tok, err := tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	_, err = chargesSvc.Create(ctx, merchantID, apikeys.ModeSandbox, tok.ID, mustMoney(t, 5_000_00), testsupport.RandomKey())
	if !errors.Is(err, charges.ErrNoSettlementAccount) {
		t.Fatalf("create charge without settlement account: got %v, want ErrNoSettlementAccount", err)
	}
}
