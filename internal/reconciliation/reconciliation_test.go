package reconciliation_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/reconciliation"
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
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, 'unused', 'Test Owner')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&identityID)
	if err != nil {
		t.Fatalf("insert identity: %v", err)
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

func TestCompletedTransferAutoMatches(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), nil)
	reconSvc := reconciliation.NewService(pool, nil)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	et, err := externalSvc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}
	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	if err := reconSvc.GenerateStatementLines(ctx); err != nil {
		t.Fatalf("generate statement lines: %v", err)
	}
	if err := reconSvc.RunReconciliation(ctx); err != nil {
		t.Fatalf("run reconciliation: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM reconciliation_statement_lines WHERE external_transfer_id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query line status: %v", err)
	}
	if status != "matched" {
		t.Fatalf("status = %s, want matched", status)
	}

	var breakCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_breaks WHERE external_transfer_id = $1`, et.ID).Scan(&breakCount); err != nil {
		t.Fatalf("count breaks: %v", err)
	}
	if breakCount != 0 {
		t.Fatalf("break count = %d, want 0", breakCount)
	}
}

func TestPendingTransferIsBenignTimingNotABreak(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), nil)
	reconSvc := reconciliation.NewService(pool, nil)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	et, err := externalSvc.SendOutbound(ctx, acc, "instant", "acct-999", mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}
	// Deliberately do NOT call ProcessPending: the transfer stays pending,
	// simulating the partner's report arriving before our own side has
	// caught up.

	if err := reconSvc.GenerateStatementLines(ctx); err != nil {
		t.Fatalf("generate statement lines: %v", err)
	}
	if err := reconSvc.RunReconciliation(ctx); err != nil {
		t.Fatalf("run reconciliation: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM reconciliation_statement_lines WHERE external_transfer_id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query line status: %v", err)
	}
	if status != "unmatched" {
		t.Fatalf("status = %s, want unmatched (benign timing, not yet resolved)", status)
	}

	var breakCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_breaks WHERE external_transfer_id = $1`, et.ID).Scan(&breakCount); err != nil {
		t.Fatalf("count breaks: %v", err)
	}
	if breakCount != 0 {
		t.Fatalf("break count = %d, want 0 (must not flag a still-pending transfer as a break)", breakCount)
	}
}

func TestSeededDiscrepancyProducesBreak(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), nil)
	reconSvc := reconciliation.NewService(pool, nil)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	et, err := externalSvc.SendOutbound(ctx, acc, "instant", "acct-999:RECONBREAK", mustMoney(t, 5_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("send outbound: %v", err)
	}
	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	if err := reconSvc.GenerateStatementLines(ctx); err != nil {
		t.Fatalf("generate statement lines: %v", err)
	}
	if err := reconSvc.RunReconciliation(ctx); err != nil {
		t.Fatalf("run reconciliation: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM reconciliation_statement_lines WHERE external_transfer_id = $1`, et.ID).Scan(&status); err != nil {
		t.Fatalf("query line status: %v", err)
	}
	if status != "break" {
		t.Fatalf("status = %s, want break", status)
	}

	var reason string
	var expected, reported int64
	if err := pool.QueryRow(ctx, `
		SELECT reason, expected_amount_minor, reported_amount_minor FROM reconciliation_breaks WHERE external_transfer_id = $1
	`, et.ID).Scan(&reason, &expected, &reported); err != nil {
		t.Fatalf("query break: %v", err)
	}
	if reason != "amount_mismatch" {
		t.Fatalf("reason = %s, want amount_mismatch", reason)
	}
	if expected != 5_000_00 {
		t.Fatalf("expected amount = %d, want 500000", expected)
	}
	if reported == expected {
		t.Fatal("expected reported amount to differ from expected")
	}
}
