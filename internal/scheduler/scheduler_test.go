package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/scheduler"
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

func newServices(pool *pgxpool.Pool) (*ledger.Service, *scheduler.Service) {
	ledgerSvc := ledger.NewService(pool)
	identitySvc := identity.NewService(pool)
	paymentsSvc := payments.NewService(pool, ledgerSvc, identitySvc, resilience.NewService(pool))
	return ledgerSvc, scheduler.NewService(pool, paymentsSvc, nil)
}

func TestOnceScheduleFiresExactlyOnce(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, schedSvc := newServices(pool)
	ctx := context.Background()

	from := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	to := newFundedAccount(t, pool, ledgerSvc, 0)

	st, err := schedSvc.Create(ctx, from, to, mustMoney(t, 5_000_00), scheduler.ScheduleOnce, nil, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	if err := schedSvc.RunDue(ctx); err != nil {
		t.Fatalf("run due: %v", err)
	}
	// Second call must be a no-op: the schedule is no longer active.
	if err := schedSvc.RunDue(ctx); err != nil {
		t.Fatalf("run due again: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, to)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 5_000_00 {
		t.Fatalf("recipient balance = %d, want 500000 (fired exactly once)", bal.Available.Minor)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM scheduled_transfers WHERE id = $1`, st.ID).Scan(&status); err != nil {
		t.Fatalf("query schedule status: %v", err)
	}
	if status != "completed" {
		t.Fatalf("schedule status = %s, want completed", status)
	}
}

func TestRecurringScheduleAdvances(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, schedSvc := newServices(pool)
	ctx := context.Background()

	from := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	to := newFundedAccount(t, pool, ledgerSvc, 0)

	daily := scheduler.IntervalDaily
	firstRun := time.Now().Add(-time.Minute)
	st, err := schedSvc.Create(ctx, from, to, mustMoney(t, 1_000_00), scheduler.ScheduleRecurring, &daily, firstRun)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	if err := schedSvc.RunDue(ctx); err != nil {
		t.Fatalf("run due: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, to)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1_000_00 {
		t.Fatalf("recipient balance = %d, want 100000 after first fire", bal.Available.Minor)
	}

	var status string
	var nextRunAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status, next_run_at FROM scheduled_transfers WHERE id = $1`, st.ID).Scan(&status, &nextRunAt); err != nil {
		t.Fatalf("query schedule: %v", err)
	}
	if status != "active" {
		t.Fatalf("schedule status = %s, want active (recurring)", status)
	}
	if !nextRunAt.After(firstRun) {
		t.Fatalf("next_run_at = %v, want after %v", nextRunAt, firstRun)
	}

	// Not due yet, so a second RunDue must not fire again.
	if err := schedSvc.RunDue(ctx); err != nil {
		t.Fatalf("run due again: %v", err)
	}
	bal, _ = ledgerSvc.GetBalance(ctx, to)
	if bal.Available.Minor != 1_000_00 {
		t.Fatalf("recipient balance after second RunDue = %d, want unchanged 100000", bal.Available.Minor)
	}
}

func TestConcurrentRunDueFiresOnlyOnce(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, schedSvc := newServices(pool)
	ctx := context.Background()

	from := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	to := newFundedAccount(t, pool, ledgerSvc, 0)

	_, err := schedSvc.Create(ctx, from, to, mustMoney(t, 2_000_00), scheduler.ScheduleOnce, nil, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = schedSvc.RunDue(ctx)
		}()
	}
	wg.Wait()

	bal, err := ledgerSvc.GetBalance(ctx, to)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 2_000_00 {
		t.Fatalf("recipient balance = %d, want 200000 (exactly one fire despite %d concurrent RunDue calls)", bal.Available.Minor, n)
	}
}

func TestResolveStuckRecoversManuallyStuckRun(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, schedSvc := newServices(pool)
	ctx := context.Background()

	from := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	to := newFundedAccount(t, pool, ledgerSvc, 0)

	scheduledFor := time.Now().Add(-time.Minute)
	st, err := schedSvc.Create(ctx, from, to, mustMoney(t, 3_000_00), scheduler.ScheduleOnce, nil, scheduledFor)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	// Simulate a crash right after a run was claimed but before it executed:
	// insert the "processing" run row directly, with attempted_at far enough
	// in the past to be picked up by ResolveStuck, and DELIBERATELY do not
	// call RunDue — this row is the only trace of that claim.
	var runID string
	err = pool.QueryRow(ctx, `
		INSERT INTO scheduled_transfer_runs (scheduled_transfer_id, scheduled_for, status, attempted_at)
		VALUES ($1, $2, 'processing', now() - interval '10 minutes')
		RETURNING id::text
	`, st.ID, st.NextRunAt).Scan(&runID)
	if err != nil {
		t.Fatalf("insert stuck run: %v", err)
	}

	if err := schedSvc.ResolveStuck(ctx); err != nil {
		t.Fatalf("resolve stuck: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, to)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 3_000_00 {
		t.Fatalf("recipient balance = %d, want 300000 (stuck run resolved to completion exactly once)", bal.Available.Minor)
	}

	var runStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM scheduled_transfer_runs WHERE id = $1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "completed" {
		t.Fatalf("run status = %s, want completed", runStatus)
	}

	// A second sweep must not re-fire it (idempotency layer + status no
	// longer 'processing').
	if err := schedSvc.ResolveStuck(ctx); err != nil {
		t.Fatalf("resolve stuck again: %v", err)
	}
	bal, _ = ledgerSvc.GetBalance(ctx, to)
	if bal.Available.Minor != 3_000_00 {
		t.Fatalf("recipient balance after second sweep = %d, want unchanged 300000", bal.Available.Minor)
	}
}
