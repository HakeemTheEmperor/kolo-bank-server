package bills_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/bills"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
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

func newFundedAccount(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service, fundMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var identityID string
	err := pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', 2, $1, 'unused', 'Test Owner')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&identityID)
	if err != nil {
		t.Fatalf("insert test identity: %v", err)
	}
	acc, err := ledgerSvc.OpenAccount(ctx, identityID, ledger.AccountTypeWallet, "NGN", 0)
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

func airtimeBillerID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM billers WHERE code = 'KOLO-AIRTIME'`).Scan(&id); err != nil {
		t.Fatalf("look up seeded airtime biller: %v", err)
	}
	return id
}

func newServices(pool *pgxpool.Pool) (*ledger.Service, *externalpayments.Service, *bills.Service) {
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(pool, ledgerSvc, registry, risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), resilience.NewService(pool), nil)
	return ledgerSvc, externalSvc, bills.NewService(pool, externalSvc, resilience.NewService(pool))
}

func TestValidateReference(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	_, _, billsSvc := newServices(pool)
	ctx := context.Background()
	billerID := airtimeBillerID(t, pool)

	valid, name, err := billsSvc.ValidateReference(ctx, billerID, "08012345678")
	if err != nil {
		t.Fatalf("validate reference: %v", err)
	}
	if !valid || name == "" {
		t.Fatalf("valid=%v name=%q, want valid with a name", valid, name)
	}

	valid, _, err = billsSvc.ValidateReference(ctx, billerID, "INVALID-123")
	if err != nil {
		t.Fatalf("validate invalid reference: %v", err)
	}
	if valid {
		t.Fatal("expected reference containing INVALID to fail validation")
	}
}

func TestPayBillEndToEnd(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, externalSvc, billsSvc := newServices(pool)
	ctx := context.Background()
	billerID := airtimeBillerID(t, pool)

	acc := newFundedAccount(t, pool, ledgerSvc, 10_000_00)

	bp, err := billsSvc.PayBill(ctx, acc, billerID, "08012345678", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("pay bill: %v", err)
	}
	if bp.ExternalTransferID == "" {
		t.Fatal("expected bill payment to link to an external transfer")
	}

	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, bp.ExternalTransferID).Scan(&status); err != nil {
		t.Fatalf("query external transfer status: %v", err)
	}
	if status != "completed" {
		t.Fatalf("external transfer status = %s, want completed", status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 9_000_00 {
		t.Fatalf("available = %d, want 900000", bal.Available.Minor)
	}
}

func TestPayBillInvalidReferenceRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, _, billsSvc := newServices(pool)
	ctx := context.Background()
	billerID := airtimeBillerID(t, pool)

	acc := newFundedAccount(t, pool, ledgerSvc, 10_000_00)

	_, err := billsSvc.PayBill(ctx, acc, billerID, "INVALID-REF", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if !errors.Is(err, bills.ErrInvalidReference) {
		t.Fatalf("pay bill with invalid reference: got %v, want ErrInvalidReference", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 10_000_00 {
		t.Fatalf("available = %d, want unchanged 1000000 (no hold placed for a rejected reference)", bal.Available.Minor)
	}
}

func TestRecurringBillFiresAndIsIdempotent(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, externalSvc, billsSvc := newServices(pool)
	ctx := context.Background()
	billerID := airtimeBillerID(t, pool)

	acc := newFundedAccount(t, pool, ledgerSvc, 10_000_00)

	r, err := billsSvc.CreateRecurring(ctx, acc, billerID, "08099999999", mustMoney(t, 500_00), bills.IntervalMonthly, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("create recurring: %v", err)
	}

	if err := billsSvc.RunDueBills(ctx); err != nil {
		t.Fatalf("run due bills: %v", err)
	}
	// A second call before the (now-advanced) next_run_at must not fire again.
	if err := billsSvc.RunDueBills(ctx); err != nil {
		t.Fatalf("run due bills again: %v", err)
	}

	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var paymentCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM bill_payments WHERE account_id = $1`, acc).Scan(&paymentCount); err != nil {
		t.Fatalf("count bill payments: %v", err)
	}
	if paymentCount != 1 {
		t.Fatalf("bill payment count = %d, want exactly 1", paymentCount)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 9_500_00 {
		t.Fatalf("available = %d, want 950000 (fired exactly once)", bal.Available.Minor)
	}

	var nextRunAt time.Time
	if err := pool.QueryRow(ctx, `SELECT next_run_at FROM recurring_bill_payments WHERE id = $1`, r.ID).Scan(&nextRunAt); err != nil {
		t.Fatalf("query next_run_at: %v", err)
	}
	if !nextRunAt.After(time.Now()) {
		t.Fatalf("next_run_at = %v, want in the future after advancing", nextRunAt)
	}
}
