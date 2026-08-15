package settlement_test

import (
	"context"
	"testing"
	"time"

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
	"github.com/toluwalase/kolo-bank-server/internal/settlement"
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

type harness struct {
	pool        *pgxpool.Pool
	ledgerSvc   *ledger.Service
	tokensSvc   *tokens.Service
	externalSvc *externalpayments.Service
	chargesSvc  *charges.Service
	payoutsSvc  *payouts.Service
	feesSvc     *fees.Service
	settleSvc   *settlement.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	pool := testsupport.RequireTestPool(t)
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(pool, ledgerSvc, registry, risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), nil), nil)
	payoutsSvc := payouts.NewService(pool, externalSvc, registry)
	return harness{
		pool:        pool,
		ledgerSvc:   ledgerSvc,
		tokensSvc:   tokens.NewService(pool),
		externalSvc: externalSvc,
		chargesSvc:  charges.NewService(pool, tokens.NewService(pool), externalSvc),
		payoutsSvc:  payoutsSvc,
		feesSvc:     fees.NewService(pool, ledgerSvc, nil),
		settleSvc:   settlement.NewService(pool, payoutsSvc, nil),
	}
}

// newMerchant inserts an active business identity with an open NGN
// settlement account — charges.Service.Create resolves a merchant's
// "earliest open account" (internal/charges/charges.go), so one must
// exist before chargeMerchant can charge anything.
func (h harness) newMerchant(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := h.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test merchant: %v", err)
	}
	if _, err := h.ledgerSvc.OpenAccount(ctx, id, ledger.AccountTypeCurrent, "NGN", 0); err != nil {
		t.Fatalf("open settlement account: %v", err)
	}
	return id
}

// chargeMerchant tokenizes and completes one charge for merchantID,
// applies fees, and returns the charge id plus the ledger transaction id
// underlying it.
func (h harness) chargeMerchant(t *testing.T, merchantID string, amountMinor int64) (chargeID, ledgerTxnID string) {
	t.Helper()
	ctx := context.Background()

	tok, err := h.tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	ch, err := h.chargesSvc.Create(ctx, merchantID, apikeys.ModeLive, tok.ID, mustMoney(t, amountMinor), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create charge: %v", err)
	}
	if err := h.externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}
	if err := h.feesSvc.ApplyFees(ctx); err != nil {
		t.Fatalf("apply fees: %v", err)
	}

	var txnID string
	if err := h.pool.QueryRow(ctx, `
		SELECT et.ledger_transaction_id::text FROM charges c JOIN external_transfers et ON et.id = c.external_transfer_id WHERE c.id = $1
	`, ch.ID).Scan(&txnID); err != nil {
		t.Fatalf("query ledger transaction id: %v", err)
	}
	return ch.ID, txnID
}

func TestRunCyclesAggregatesAndPaysOutNet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	merchantID := h.newMerchant(t)

	_, _ = h.chargeMerchant(t, merchantID, 5_000_00)
	_, _ = h.chargeMerchant(t, merchantID, 3_000_00)

	var gross, feesTotal int64
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(c.amount_minor), 0), COALESCE(SUM(fc.total_minor), 0)
		FROM charges c JOIN fee_charges fc ON fc.source_type = 'charge' AND fc.source_id = c.id
		WHERE c.merchant_id = $1
	`, merchantID).Scan(&gross, &feesTotal); err != nil {
		t.Fatalf("sum charges: %v", err)
	}
	net := gross - feesTotal
	reserve := net * 1000 / 10000 // 10%
	wantPayout := net - reserve

	if _, err := h.settleSvc.CreateConfig(ctx, merchantID, "NGN", 1000, 7, "recipient-bank-acct", "instant", settlement.IntervalDaily, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create config: %v", err)
	}

	if err := h.settleSvc.RunCycles(ctx); err != nil {
		t.Fatalf("run cycles: %v", err)
	}

	var cycleID string
	var gotGross, gotFees, gotReserve, gotNet int64
	var payoutID *string
	if err := h.pool.QueryRow(ctx, `
		SELECT id::text, gross_minor, fees_minor, reserve_minor, net_minor, payout_id::text FROM settlement_cycles WHERE merchant_id = $1
	`, merchantID).Scan(&cycleID, &gotGross, &gotFees, &gotReserve, &gotNet, &payoutID); err != nil {
		t.Fatalf("query cycle: %v", err)
	}
	if gotGross != gross || gotFees != feesTotal || gotReserve != reserve || gotNet != net {
		t.Fatalf("cycle = gross=%d fees=%d reserve=%d net=%d, want gross=%d fees=%d reserve=%d net=%d",
			gotGross, gotFees, gotReserve, gotNet, gross, feesTotal, reserve, net)
	}
	if payoutID == nil {
		t.Fatal("expected a payout to have been created for the cycle")
	}

	if err := h.externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process settlement payout: %v", err)
	}

	p, err := h.payoutsSvc.Get(ctx, *payoutID)
	if err != nil {
		t.Fatalf("get payout: %v", err)
	}
	if p.Amount.Minor != wantPayout {
		t.Fatalf("payout amount = %d, want %d (net minus reserve)", p.Amount.Minor, wantPayout)
	}
}

func TestReversedChargeExcludedFromSettlement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	merchantID := h.newMerchant(t)

	_, ledgerTxnID := h.chargeMerchant(t, merchantID, 5_000_00)

	// A real reversal would fail here: the fee already applied to this
	// charge means the merchant's available balance can no longer cover
	// reversing the full original amount (already covered by
	// internal/ledger's own reversal tests). This test is only
	// verifying settlement's own filter on lt.state, so set it directly.
	if _, err := h.pool.Exec(ctx, `UPDATE ledger_transactions SET state = 'reversed' WHERE id = $1`, ledgerTxnID); err != nil {
		t.Fatalf("mark transaction reversed: %v", err)
	}

	if _, err := h.settleSvc.CreateConfig(ctx, merchantID, "NGN", 0, 7, "recipient-bank-acct", "instant", settlement.IntervalDaily, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := h.settleSvc.RunCycles(ctx); err != nil {
		t.Fatalf("run cycles: %v", err)
	}

	var count int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM settlement_cycles WHERE merchant_id = $1`, merchantID).Scan(&count); err != nil {
		t.Fatalf("count cycles: %v", err)
	}
	if count != 0 {
		t.Fatalf("cycle count = %d, want 0 (reversed charge must not be settled)", count)
	}
}

func TestReleaseMaturedReserves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	merchantID := h.newMerchant(t)

	h.chargeMerchant(t, merchantID, 10_000_00)

	if _, err := h.settleSvc.CreateConfig(ctx, merchantID, "NGN", 2000, 7, "recipient-bank-acct", "instant", settlement.IntervalDaily, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := h.settleSvc.RunCycles(ctx); err != nil {
		t.Fatalf("run cycles: %v", err)
	}

	var cycleID string
	var reserveMinor int64
	if err := h.pool.QueryRow(ctx, `SELECT id::text, reserve_minor FROM settlement_cycles WHERE merchant_id = $1`, merchantID).Scan(&cycleID, &reserveMinor); err != nil {
		t.Fatalf("query cycle: %v", err)
	}
	if reserveMinor <= 0 {
		t.Fatalf("reserve = %d, want > 0", reserveMinor)
	}

	// Not yet released: hold period hasn't passed.
	if err := h.settleSvc.ReleaseMaturedReserves(ctx); err != nil {
		t.Fatalf("release matured reserves (too early): %v", err)
	}
	c, err := h.settleSvc.GetCycle(ctx, cycleID)
	if err != nil {
		t.Fatalf("get cycle: %v", err)
	}
	if c.ReserveReleased {
		t.Fatal("expected reserve to not be released before its hold period")
	}

	if _, err := h.pool.Exec(ctx, `UPDATE settlement_cycles SET reserve_release_at = now() - interval '1 minute' WHERE id = $1`, cycleID); err != nil {
		t.Fatalf("backdate reserve_release_at: %v", err)
	}

	if err := h.settleSvc.ReleaseMaturedReserves(ctx); err != nil {
		t.Fatalf("release matured reserves: %v", err)
	}
	c, err = h.settleSvc.GetCycle(ctx, cycleID)
	if err != nil {
		t.Fatalf("get cycle: %v", err)
	}
	if !c.ReserveReleased {
		t.Fatal("expected reserve to be released after its hold period")
	}

	var payoutCount int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE merchant_id = $1`, merchantID).Scan(&payoutCount); err != nil {
		t.Fatalf("count payouts: %v", err)
	}
	if payoutCount != 2 { // the cycle's net payout + the reserve release payout
		t.Fatalf("payout count = %d, want 2", payoutCount)
	}

	// Calling again must not double-release.
	if err := h.settleSvc.ReleaseMaturedReserves(ctx); err != nil {
		t.Fatalf("release matured reserves again: %v", err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE merchant_id = $1`, merchantID).Scan(&payoutCount); err != nil {
		t.Fatalf("count payouts again: %v", err)
	}
	if payoutCount != 2 {
		t.Fatalf("payout count after second release = %d, want unchanged 2", payoutCount)
	}
}
