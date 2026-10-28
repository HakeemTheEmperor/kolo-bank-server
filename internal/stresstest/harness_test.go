//go:build stress

// Package stresstest holds the measurement tests behind METRICS.md:
// concurrency, idempotency, hold races, crash recovery and webhook
// retries, each run against a real Postgres. They are slow and print
// numbers rather than just pass/fail, so they sit behind the "stress"
// build tag and never run in the normal `go test ./...`.
//
// Every test that has a protection worth measuring runs it twice, once
// as production ships and once with that protection removed. Removal is
// always test-side: a swapped SQL function restored on cleanup, a build
// tag (stress_nolock), or a client that skips idempotency keys. Nothing
// here changes how the production binary behaves.
//
// Results are appended as JSON lines to $STRESS_RESULTS (if set) so a run
// can be turned into tables without copying numbers by hand.
package stresstest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// defaultPoolMaxConns is deliberately larger than pgxpool's default
// (max(4, NumCPU)): with only 4 connections, 500 goroutines mostly queue
// for a connection instead of racing inside Postgres, which hides exactly
// the races these tests exist to measure.
const defaultPoolMaxConns = 50

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// requirePool returns a migrated pool sized by STRESS_POOL_MAX_CONNS.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testsupport.RequireTestPool(t) // skips without DATABASE_URL; runs migrations

	poolOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
		if err != nil {
			poolErr = err
			return
		}
		cfg.MaxConns = int32(envInt("STRESS_POOL_MAX_CONNS", defaultPoolMaxConns))
		pool, poolErr = pgxpool.NewWithConfig(context.Background(), cfg)
	})
	if poolErr != nil {
		t.Fatalf("stresstest: pool: %v", poolErr)
	}
	return pool
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// runs is how many times each scenario repeats; tables report the range.
func runs() int { return envInt("STRESS_RUNS", 5) }

// result is one measured scenario run.
type result struct {
	Test       string         `json:"test"`
	Scenario   string         `json:"scenario"`
	Protection string         `json:"protection"`
	Run        int            `json:"run"`
	Metrics    map[string]any `json:"metrics"`
	At         time.Time      `json:"at"`
}

var resultsMu sync.Mutex

// record logs r and appends it to $STRESS_RESULTS.
func record(t *testing.T, r result) {
	t.Helper()
	r.At = time.Now().UTC()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	t.Logf("RESULT %s", b)

	path := os.Getenv("STRESS_RESULTS")
	if path == "" {
		return
	}
	resultsMu.Lock()
	defer resultsMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open results file: %v", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", b); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func ngn(minor int64) ledger.Money { return ledger.Money{Minor: minor, Currency: "NGN"} }

// newOwner inserts a minimal active identity, the same shortcut the
// existing package tests take instead of running onboarding.
func newOwner(t *testing.T, p *pgxpool.Pool, kycTier int) string {
	t.Helper()
	var id string
	err := p.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, $2, 'unused', 'Stress Test Owner')
		RETURNING id::text
	`, kycTier, testsupport.RandomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	return id
}

// newFundedAccount opens a wallet and credits it with balance minor units.
func newFundedAccount(t *testing.T, p *pgxpool.Pool, svc *ledger.Service, balance int64) ledger.Account {
	t.Helper()
	ctx := context.Background()
	acc, err := svc.OpenAccount(ctx, newOwner(t, p, 2), ledger.AccountTypeWallet, "NGN", 0)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if balance > 0 {
		if _, err := svc.Credit(ctx, acc.ID, ngn(balance), testsupport.RandomKey()); err != nil {
			t.Fatalf("fund account: %v", err)
		}
	}
	return acc
}

func ledgerBalance(t *testing.T, p *pgxpool.Pool, accountID string) int64 {
	t.Helper()
	var bal int64
	if err := p.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries WHERE account_id = $1`, accountID,
	).Scan(&bal); err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	return bal
}

// startTogether runs n copies of fn, released at the same instant, and
// waits for all of them.
func startTogether(n int, fn func(i int)) time.Duration {
	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-gate
			fn(i)
		}(i)
	}
	ready.Wait()
	start := time.Now()
	close(gate)
	done.Wait()
	return time.Since(start)
}

// swapFunction replaces a plpgsql function's body for the rest of the
// test, restoring the original definition on cleanup. It's how the tests
// remove a database-side protection without touching the migrations.
func swapFunction(t *testing.T, p *pgxpool.Pool, name string, edit func(def string) string) {
	t.Helper()
	ctx := context.Background()
	var original string
	if err := p.QueryRow(ctx, `SELECT pg_get_functiondef($1::regproc)`, name).Scan(&original); err != nil {
		t.Fatalf("read function %s: %v", name, err)
	}
	modified := edit(original)
	if modified == original {
		t.Fatalf("swapFunction %s: edit changed nothing", name)
	}
	if _, err := p.Exec(ctx, modified); err != nil {
		t.Fatalf("install modified %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), original); err != nil {
			t.Errorf("RESTORE FAILED for %s: %v", name, err)
		}
	})
}
