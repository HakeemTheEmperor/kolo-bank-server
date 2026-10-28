//go:build stress

package stresstest

import (
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// TestCrashRecovery SIGKILLs a real worker process (./crashworker) at
// random moments while it submits and processes outbound external
// transfers, restarts it each time, then waits for everything to settle
// and audits the result.
func TestCrashRecovery(t *testing.T) {
	const (
		accounts   = 100
		perAccount = 4 // risk's velocity rule holds the 5th transfer in 10 minutes
		kills      = 25
		settleMax  = 8 * time.Minute
	)
	p := requirePool(t)
	svc := ledger.NewService(p)

	bin := filepath.Join(t.TempDir(), "crashworker")
	build := exec.Command("go", "build", "-tags", "stress", "-o", bin, "./crashworker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build crashworker: %v\n%s", err, out)
	}

	for run := 1; run <= envInt("STRESS_CRASH_RUNS", 1); run++ {
		ids := make([]string, accounts)
		for i := range ids {
			ids[i] = newFundedAccount(t, p, svc, 10_000).ID
		}
		accountsFile := filepath.Join(t.TempDir(), "accounts.txt")
		if err := os.WriteFile(accountsFile, []byte(strings.Join(ids, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		runID := testsupport.RandomKey()
		total := accounts * perAccount

		start := func() *exec.Cmd {
			cmd := exec.Command(bin, "-accounts", accountsFile, "-per-account", "4", "-run-id", runID)
			cmd.Env = os.Environ()
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start worker: %v", err)
			}
			return cmd
		}

		rng := rand.New(rand.NewSource(int64(run)))
		interrupted := map[string]bool{}
		maxInFlightAtKill := 0
		for k := 0; k < kills; k++ {
			cmd := start()
			time.Sleep(time.Duration(300+rng.Intn(1200)) * time.Millisecond)
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			inFlight := processingIDs(t, p, runID)
			for _, id := range inFlight {
				interrupted[id] = true
			}
			if len(inFlight) > maxInFlightAtKill {
				maxInFlightAtKill = len(inFlight)
			}
		}

		// Last restart: let it run until every transfer is terminal.
		restartedAt := time.Now()
		cmd := start()
		var recovery time.Duration
		settled := false
		for time.Since(restartedAt) < settleMax {
			rows, open := transferProgress(t, p, runID)
			if rows == total && open == 0 {
				recovery = time.Since(restartedAt)
				settled = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()

		audit := auditExternal(t, p, runID, ids)
		audit["transfers_submitted"] = total
		audit["kills"] = kills
		audit["operations_interrupted_mid_transfer"] = len(interrupted)
		audit["max_in_flight_at_a_kill"] = maxInFlightAtKill
		audit["settled"] = settled
		audit["recovery_seconds_after_last_restart"] = recovery.Seconds()
		record(t, result{Test: "crash_recovery", Scenario: "SIGKILL worker 25 times during 400 outbound transfers", Protection: "as_shipped", Run: run, Metrics: audit})
	}
}

func processingIDs(t *testing.T, p *pgxpool.Pool, runID string) []string {
	t.Helper()
	rows, err := p.Query(context.Background(),
		`SELECT id::text FROM external_transfers WHERE idempotency_key LIKE $1 AND status = 'processing'`, "crash-"+runID+"-%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func transferProgress(t *testing.T, p *pgxpool.Pool, runID string) (rows, open int) {
	t.Helper()
	if err := p.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE status NOT IN ('completed', 'failed'))
		FROM external_transfers WHERE idempotency_key LIKE $1
	`, "crash-"+runID+"-%").Scan(&rows, &open); err != nil {
		t.Fatal(err)
	}
	return
}

// auditExternal checks every transfer whose key starts with crash-<runID>
// against its hold and its ledger postings.
func auditExternal(t *testing.T, p *pgxpool.Pool, runID string, accountIDs []string) map[string]any {
	t.Helper()
	ctx := context.Background()
	out := map[string]any{}
	var completed, failed, stuck, doublePosted, completedBadHold, failedBadHold, failedWithPosting int
	if err := p.QueryRow(ctx, `
		WITH et AS (
			SELECT et.*, h.status AS hold_status,
			       (SELECT count(*) FROM ledger_transactions lt WHERE lt.idempotency_key = et.idempotency_key) AS postings
			FROM external_transfers et LEFT JOIN holds h ON h.id = et.hold_id
			WHERE et.idempotency_key LIKE $1
		)
		SELECT count(*) FILTER (WHERE status = 'completed'),
		       count(*) FILTER (WHERE status = 'failed'),
		       count(*) FILTER (WHERE status NOT IN ('completed', 'failed')),
		       count(*) FILTER (WHERE postings > 1),
		       count(*) FILTER (WHERE status = 'completed' AND (hold_status <> 'captured' OR postings <> 1)),
		       count(*) FILTER (WHERE status = 'failed' AND hold_status <> 'released'),
		       count(*) FILTER (WHERE status = 'failed' AND postings > 0)
		FROM et
	`, "crash-"+runID+"-%").Scan(&completed, &failed, &stuck, &doublePosted, &completedBadHold, &failedBadHold, &failedWithPosting); err != nil {
		t.Fatal(err)
	}
	out["completed"] = completed
	out["failed"] = failed
	out["stuck_not_final"] = stuck
	out["double_postings"] = doublePosted
	out["completed_without_exactly_one_capture"] = completedBadHold
	out["failed_without_released_hold"] = failedBadHold
	out["failed_but_posted"] = failedWithPosting

	var orphanCount int
	var orphanAmount int64
	for _, id := range accountIDs {
		n, amt := orphanHolds(t, p, id)
		orphanCount += n
		orphanAmount += amt
	}
	out["orphan_holds"] = orphanCount
	out["orphan_reserved_minor"] = orphanAmount

	// Money check: each account started at 10000 and should be down by
	// exactly the completed transfers' amounts.
	var mismatched int
	for _, id := range accountIDs {
		var debited int64
		if err := p.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_minor), 0) FROM external_transfers
			WHERE account_id = $1 AND status = 'completed' AND idempotency_key LIKE $2
		`, id, "crash-"+runID+"-%").Scan(&debited); err != nil {
			t.Fatal(err)
		}
		if ledgerBalance(t, p, id) != 10_000-debited {
			mismatched++
		}
	}
	out["accounts_with_wrong_balance"] = mismatched
	return out
}

// TestCrashPoints builds, for each step boundary in the outbound flow, the
// exact database state a crash at that point leaves behind, then runs the
// production resolver and checks the outcome. attempted_at is backdated
// past stuckTimeout to stand in for the two minutes a real resolver
// waits; nothing else is altered.
func TestCrashPoints(t *testing.T) {
	const perPoint = 20
	p := requirePool(t)
	ledgerSvc := ledger.NewService(p)
	ext := newExternalService(p, ledgerSvc)
	ctx := context.Background()

	// recordAssessment writes the risk_assessments row exactly as
	// risk.Service.record does, i.e. the state right before Assess applies
	// its decision to the transfer.
	recordAssessment := func(t *testing.T, transferID, decision string, score int) {
		if _, err := p.Exec(ctx, `
			INSERT INTO risk_assessments (source_type, source_id, score, decision, reasons)
			VALUES ('external_transfer', $1, $2, $3, '["crash-point"]')
		`, transferID, score, decision); err != nil {
			t.Fatalf("record assessment: %v", err)
		}
	}

	type point struct {
		name   string
		ref    string
		want   string // final status the transfer should reach: completed, failed or held
		before func(t *testing.T, transferID, holdID, key string)
	}
	points := []point{
		{"after_claim_before_rail_call", "ACCT-OK", "completed", func(*testing.T, string, string, string) {}},
		{"after_capture_before_mark_completed", "ACCT-OK", "completed", func(t *testing.T, _, holdID, key string) {
			if _, err := ledgerSvc.CaptureHold(ctx, holdID, key); err != nil {
				t.Fatalf("capture: %v", err)
			}
		}},
		{"after_release_before_mark_failed", "ACCT-RAILFAIL", "failed", func(t *testing.T, _, holdID, _ string) {
			if err := ledgerSvc.ReleaseHold(ctx, holdID); err != nil {
				t.Fatalf("release: %v", err)
			}
		}},
		{"after_risk_block_recorded_before_applied", "ACCT-FRAUDSCORE", "failed", func(t *testing.T, id, _, _ string) {
			recordAssessment(t, id, "block", 100)
		}},
		{"after_risk_hold_recorded_before_applied", "ACCT-OK", "held", func(t *testing.T, id, _, _ string) {
			recordAssessment(t, id, "hold", 70)
		}},
	}

	for _, pt := range points {
		var keys []string
		var ids []string
		for i := 0; i < perPoint; i++ {
			acc := newFundedAccount(t, p, ledgerSvc, 10_000)
			ids = append(ids, acc.ID)
			key := "crashpoint-" + testsupport.RandomKey()
			et, err := ext.SendOutbound(ctx, acc.ID, "instant", pt.ref, ngn(250), key)
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			pt.before(t, et.ID, *et.HoldID, key)
			if _, err := p.Exec(ctx, `
				UPDATE external_transfers SET status = 'processing', attempted_at = now() - interval '3 minutes' WHERE id = $1
			`, et.ID); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, key)
		}

		// Three resolver passes, each preceded by re-backdating whatever is
		// still processing, i.e. six minutes of real resolver time.
		for pass := 0; pass < 3; pass++ {
			if err := ext.ResolveStuck(ctx); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if _, err := p.Exec(ctx, `
				UPDATE external_transfers SET attempted_at = now() - interval '3 minutes'
				WHERE status = 'processing' AND idempotency_key = ANY($1)
			`, keys); err != nil {
				t.Fatal(err)
			}
		}

		var double int
		if err := p.QueryRow(ctx, `
			SELECT count(*) FROM external_transfers et WHERE idempotency_key = ANY($1)
			  AND (SELECT count(*) FROM ledger_transactions lt WHERE lt.idempotency_key = et.idempotency_key) > 1
		`, keys).Scan(&double); err != nil {
			t.Fatal(err)
		}
		outcomes := map[string]int{}
		matched, wrong := 0, 0
		for i, id := range ids {
			var status, riskStatus string
			if err := p.QueryRow(ctx, `SELECT status, risk_status FROM external_transfers WHERE idempotency_key = $1`, keys[i]).Scan(&status, &riskStatus); err != nil {
				t.Fatal(err)
			}
			got := status
			if status == "pending" && riskStatus == "held" {
				got = "held"
			}
			outcomes[got]++
			if got == pt.want {
				matched++
			}
			want := int64(10_000)
			if status == "completed" {
				want -= 250
			}
			if ledgerBalance(t, p, id) != want {
				wrong++
			}
		}
		stillActive := 0
		for _, id := range ids {
			_, a, _ := holdStats(t, p, id)
			stillActive += a
		}
		record(t, result{Test: "crash_recovery", Scenario: "crash point: " + pt.name, Protection: "as_shipped", Run: 1,
			Metrics: map[string]any{"transfers": perPoint, "expected_outcome": pt.want, "reached_expected_outcome": matched,
				"outcomes": outcomes, "double_postings": double, "accounts_with_wrong_balance": wrong, "active_holds_left": stillActive}})
	}
}
