package externalpayments_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
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

func newServices(pool *pgxpool.Pool) (*ledger.Service, *externalpayments.Service) {
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	return ledgerSvc, externalpayments.NewService(pool, ledgerSvc, registry, risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), nil)
}

func TestOutboundSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)

	et, err := svc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}
	if et.Status != externalpayments.StatusPending {
		t.Fatalf("status = %s, want pending", et.Status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 10_000_00 || bal.Available.Minor != 90_000_00 {
		t.Fatalf("pre-process balance: pending=%d available=%d, want 1000000/9000000", bal.Pending.Minor, bal.Available.Minor)
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusCompleted) {
		t.Fatalf("status = %s, want completed", status)
	}

	bal, err = ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 90_000_00 {
		t.Fatalf("post-process balance: pending=%d available=%d, want 0/9000000", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestOutboundRailRejectionReleasesHold(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)

	et, err := svc.SendOutbound(ctx, acc, "instant", "acct-RAILFAIL-999", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusFailed) {
		t.Fatalf("status = %s, want failed", status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 100_000_00 {
		t.Fatalf("balance after rejection: pending=%d available=%d, want 0/10000000 (hold released, nothing debited)", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestInboundSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 0)

	if _, err := svc.SendInbound(ctx, acc, "ach", "sender-ref-1", mustMoney(t, 25_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("send inbound: %v", err)
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 25_000_00 {
		t.Fatalf("available = %d, want 2500000", bal.Available.Minor)
	}
}

func TestBatchItemsProcessIndependently(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)

	items := []externalpayments.OutboundItem{
		{AccountID: acc, RailName: "instant", CounterpartyRef: "good-1", Amount: mustMoney(t, 1_000_00), IdempotencyKey: testsupport.RandomKey()},
		{AccountID: acc, RailName: "instant", CounterpartyRef: "RAILFAIL-2", Amount: mustMoney(t, 1_000_00), IdempotencyKey: testsupport.RandomKey()},
		{AccountID: acc, RailName: "instant", CounterpartyRef: "good-3", Amount: mustMoney(t, 1_000_00), IdempotencyKey: testsupport.RandomKey()},
	}

	batch, err := svc.CreateBatch(ctx, items)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("batch size = %d, want 3", len(batch))
	}
	if batch[0].BatchID == nil || *batch[0].BatchID != *batch[1].BatchID {
		t.Fatal("expected all batch items to share a batch id")
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var completed, failed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM external_transfers WHERE batch_id = $1 AND status = 'completed'`, *batch[0].BatchID).Scan(&completed); err != nil {
		t.Fatalf("count completed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM external_transfers WHERE batch_id = $1 AND status = 'failed'`, *batch[0].BatchID).Scan(&failed); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if completed != 2 || failed != 1 {
		t.Fatalf("completed=%d failed=%d, want 2/1", completed, failed)
	}
}

func TestOutboundTimeoutIsResolvedByResolveStuck(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)

	// "wire" is configured with 1.2s latency; a much shorter deadline here
	// forces rail.Send to hit ctx.Done() first, simulating a rail timeout.
	et, err := svc.SendOutbound(ctx, acc, "wire", "acct-999", mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := svc.ProcessPending(shortCtx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusProcessing) {
		t.Fatalf("status after timeout = %s, want processing (left for the resolver)", status)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 5_000_00 {
		t.Fatalf("pending after timeout = %d, want 500000 (hold still active, not yet resolved)", bal.Pending.Minor)
	}

	// Simulate enough time having passed for ResolveStuck's timeout window.
	if _, err := pool.Exec(ctx, `UPDATE external_transfers SET attempted_at = now() - interval '10 minutes' WHERE id = $1`, et.ID); err != nil {
		t.Fatalf("backdate attempted_at: %v", err)
	}

	if err := svc.ResolveStuck(ctx); err != nil {
		t.Fatalf("resolve stuck: %v", err)
	}

	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusCompleted) {
		t.Fatalf("status after resolve = %s, want completed", status)
	}

	bal, err = ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 95_000_00 {
		t.Fatalf("post-resolve balance: pending=%d available=%d, want 0/9500000 (posted exactly once)", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestHeldTransferIsNotClaimedUntilReleased(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	riskSvc := risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil)
	svc := externalpayments.NewService(pool, ledgerSvc, registry, riskSvc, nil)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_000_00)

	// A single transfer at/above risk's large-amount threshold is held for
	// review rather than finalized (docs/banking-backend-spec.md §3.8).
	et, err := svc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 10_000_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	var status, riskStatus string
	if err := pool.QueryRow(ctx, `SELECT status, risk_status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status, &riskStatus); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusPending) || riskStatus != "held" {
		t.Fatalf("status=%s risk_status=%s, want pending/held", status, riskStatus)
	}

	// A second tick must not claim it either.
	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending (second tick): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusPending) {
		t.Fatalf("status after second tick = %s, want still pending (held transfer must stay untouched)", status)
	}

	if err := riskSvc.Approve(ctx, et.ID); err != nil {
		t.Fatalf("release hold: %v", err)
	}

	if err := svc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending (after release): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(externalpayments.StatusCompleted) {
		t.Fatalf("status after release = %s, want completed", status)
	}
}

func TestSendOutboundIsIdempotent(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, svc := newServices(pool)
	ctx := context.Background()

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	key := testsupport.RandomKey()

	first, err := svc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 5_000_00), key)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	second, err := svc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 5_000_00), key)
	if err != nil {
		t.Fatalf("retried send: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("retried send produced a different transfer: %s != %s", first.ID, second.ID)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 5_000_00 {
		t.Fatalf("pending = %d, want 500000 (only one hold placed)", bal.Pending.Minor)
	}
}
