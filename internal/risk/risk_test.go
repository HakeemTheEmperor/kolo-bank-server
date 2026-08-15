package risk_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
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

// placeHeldTransfer inserts an external_transfers row directly (bypassing
// externalpayments) so risk.Service can be exercised as a unit, without
// pulling in the whole claim/finalize pipeline. It's deliberately inserted
// as already 'completed' rather than the real pipeline's 'pending': risk
// tests share the same live Postgres instance with every other package's
// tests in one `go test ./...` run (no reset between packages — see
// internal/testsupport's migration-lock comment), and internal/risk.Assess
// doesn't care about external_transfers.status, so leaving these rows
// genuinely 'pending' would let an unrelated externalpayments test's
// ProcessPending (which claims globally, by design) pick up this leftover
// row and try to actually process it.
func placeHeldTransfer(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service, accountID, counterpartyRef string, amountMinor int64) string {
	t.Helper()
	ctx := context.Background()

	var holdID *string
	if amountMinor > 0 {
		hold, err := ledgerSvc.PlaceHold(ctx, accountID, mustMoney(t, amountMinor))
		if err != nil {
			t.Fatalf("place hold: %v", err)
		}
		holdID = &hold.ID
	}

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO external_transfers (direction, account_id, rail_name, counterparty_ref, amount_minor, currency, idempotency_key, hold_id, status)
		VALUES ('outbound', $1, 'instant', $2, $3, 'NGN', $4, $5, 'completed')
		RETURNING id::text
	`, accountID, counterpartyRef, amountMinor, testsupport.RandomKey(), holdID).Scan(&id)
	if err != nil {
		t.Fatalf("insert external transfer: %v", err)
	}
	return id
}

func newRiskService(pool *pgxpool.Pool, ledgerSvc *ledger.Service) *risk.Service {
	return risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil)
}

func TestAssessSanctionsHitBlocksAndReleasesHold(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-SANCTIONED-1", 1_000_00)

	a, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Decision != risk.DecisionBlock {
		t.Fatalf("decision = %s, want block", a.Decision)
	}

	var status, riskStatus string
	if err := pool.QueryRow(ctx, `SELECT status, risk_status FROM external_transfers WHERE id = $1`, transferID).Scan(&status, &riskStatus); err != nil {
		t.Fatalf("query transfer: %v", err)
	}
	if status != "failed" || riskStatus != "blocked" {
		t.Fatalf("status=%s risk_status=%s, want failed/blocked", status, riskStatus)
	}

	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 100_000_00 {
		t.Fatalf("balance after block: pending=%d available=%d, want 0/10000000 (hold released)", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestAssessFraudMarkerBlocks(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-FRAUDSCORE-1", 1_000_00)

	a, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Decision != risk.DecisionBlock {
		t.Fatalf("decision = %s, want block", a.Decision)
	}
	if len(a.Reasons) == 0 || a.Reasons[0] != "fraud_signal" {
		t.Fatalf("reasons = %v, want [fraud_signal]", a.Reasons)
	}
}

func TestAssessLargeAmountHolds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-999", 10_000_000_00)

	a, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Decision != risk.DecisionHold {
		t.Fatalf("decision = %s, want hold", a.Decision)
	}

	var status, riskStatus string
	if err := pool.QueryRow(ctx, `SELECT status, risk_status FROM external_transfers WHERE id = $1`, transferID).Scan(&status, &riskStatus); err != nil {
		t.Fatalf("query transfer: %v", err)
	}
	if riskStatus != "held" {
		t.Fatalf("risk_status = %s, want held", riskStatus)
	}

	// The hold funds must still be reserved, not released, while held.
	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 10_000_000_00 {
		t.Fatalf("pending = %d, want 1000000000 (hold must remain in place while held)", bal.Pending.Minor)
	}
}

func TestAssessVelocityExceededHolds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)

	// 5 small transfers pass; the 6th within the velocity window trips the
	// count threshold.
	var lastID string
	for i := 0; i < 6; i++ {
		lastID = placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-999", 100_00)
	}

	a, err := riskSvc.Assess(ctx, lastID)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Decision != risk.DecisionHold {
		t.Fatalf("decision = %s, want hold (velocity exceeded)", a.Decision)
	}
}

func TestAssessNormalTransferAllowed(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-999", 1_000_00)

	a, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Decision != risk.DecisionAllow {
		t.Fatalf("decision = %s, want allow", a.Decision)
	}

	var riskStatus string
	if err := pool.QueryRow(ctx, `SELECT risk_status FROM external_transfers WHERE id = $1`, transferID).Scan(&riskStatus); err != nil {
		t.Fatalf("query transfer: %v", err)
	}
	if riskStatus != "clear" {
		t.Fatalf("risk_status = %s, want clear", riskStatus)
	}
}

func TestAssessIsIdempotent(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-SANCTIONED-1", 1_000_00)

	first, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("first assess: %v", err)
	}
	second, err := riskSvc.Assess(ctx, transferID)
	if err != nil {
		t.Fatalf("second assess: %v", err)
	}
	if first.Decision != second.Decision || first.Score != second.Score {
		t.Fatalf("second assess produced a different result: %+v vs %+v", first, second)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM risk_assessments WHERE source_id = $1`, transferID).Scan(&count); err != nil {
		t.Fatalf("count assessments: %v", err)
	}
	if count != 1 {
		t.Fatalf("assessment count = %d, want 1 (idempotent, not re-scored)", count)
	}

	// A second Assess for an already-blocked transfer must not attempt to
	// release the ledger hold a second time (ledger.Service.ReleaseHold on
	// an already-released hold would error).
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, transferID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %s, want failed", status)
	}
}

func TestApproveClearsAndBlockEscalates(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_000_00)
	heldID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-999", 10_000_000_00)
	if _, err := riskSvc.Assess(ctx, heldID); err != nil {
		t.Fatalf("assess: %v", err)
	}

	if err := riskSvc.Approve(ctx, heldID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var riskStatus string
	if err := pool.QueryRow(ctx, `SELECT risk_status FROM external_transfers WHERE id = $1`, heldID).Scan(&riskStatus); err != nil {
		t.Fatalf("query transfer: %v", err)
	}
	if riskStatus != "clear" {
		t.Fatalf("risk_status after approve = %s, want clear (Approve unblocks the transfer to proceed; the hold itself stays active until externalpayments finalizes it)", riskStatus)
	}
	bal, err := ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 10_000_000_00 {
		t.Fatalf("pending after approve = %d, want 1000000000 (approve clears risk_status, it does not itself release funds)", bal.Pending.Minor)
	}

	if err := riskSvc.Approve(ctx, heldID); err != risk.ErrNotHeld {
		t.Fatalf("second approve err = %v, want ErrNotHeld", err)
	}

	blockedID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-999", 10_000_000_00)
	if _, err := riskSvc.Assess(ctx, blockedID); err != nil {
		t.Fatalf("assess: %v", err)
	}
	if err := riskSvc.Block(ctx, blockedID); err != nil {
		t.Fatalf("block: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM external_transfers WHERE id = $1`, blockedID).Scan(&status); err != nil {
		t.Fatalf("query transfer: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %s, want failed", status)
	}
	bal, err = ledgerSvc.GetBalance(ctx, acc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	// blockedID's hold was released by Block; heldID's hold is still active
	// (approved, but never finalized in this test), so only its amount
	// remains pending.
	if bal.Pending.Minor != 10_000_000_00 {
		t.Fatalf("pending = %d, want 1000000000 (only heldID's still-active hold)", bal.Pending.Minor)
	}
}

func TestGenerateSARReportIsIdempotentPerPeriod(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	ledgerSvc := ledger.NewService(pool)
	riskSvc := newRiskService(pool, ledgerSvc)

	acc := newFundedAccount(t, pool, ledgerSvc, 100_000_00)
	transferID := placeHeldTransfer(t, pool, ledgerSvc, acc, "acct-SANCTIONED-1", 1_000_00)
	if _, err := riskSvc.Assess(ctx, transferID); err != nil {
		t.Fatalf("assess: %v", err)
	}

	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)

	first, err := riskSvc.GenerateSARReport(ctx, start, end)
	if err != nil {
		t.Fatalf("generate sar report: %v", err)
	}

	var payload struct {
		FlaggedTransfers []struct {
			TransferID string `json:"transfer_id"`
		} `json:"flagged_transfers"`
	}
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	found := false
	for _, l := range payload.FlaggedTransfers {
		if l.TransferID == transferID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected blocked transfer %s in SAR report, got %+v", transferID, payload.FlaggedTransfers)
	}

	second, err := riskSvc.GenerateSARReport(ctx, start, end)
	if err != nil {
		t.Fatalf("regenerate sar report: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("regenerating for the same period produced a new report: %s != %s", second.ID, first.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM regulatory_reports WHERE report_type = 'sar'`).Scan(&count); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	if count != 1 {
		t.Fatalf("report count = %d, want 1", count)
	}
}
