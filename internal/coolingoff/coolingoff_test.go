package coolingoff_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/coolingoff"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
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

type harness struct {
	pool          *pgxpool.Pool
	ledgerSvc     *ledger.Service
	identitySvc   *identity.Service
	resilienceSvc *resilience.Service
	svc           *coolingoff.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	pool := testsupport.RequireTestPool(t)
	ledgerSvc := ledger.NewService(pool)
	identitySvc := identity.NewService(pool)
	resilienceSvc := resilience.NewService(pool)
	paymentsSvc := payments.NewService(pool, ledgerSvc, identitySvc, resilienceSvc)
	return harness{
		pool: pool, ledgerSvc: ledgerSvc, identitySvc: identitySvc, resilienceSvc: resilienceSvc,
		svc: coolingoff.NewService(pool, ledgerSvc, paymentsSvc, identitySvc, resilienceSvc, nil),
	}
}

// newTierAccount inserts an identity at kyc_tier 2 (high limits, keeps
// limit-related tests focused on cooling-off's own risk logic rather than
// tier ceilings) with a funded open account.
func (h harness) newTierAccount(t *testing.T, legalName string, fundMinor int64) (identityID, email, accountID string) {
	t.Helper()
	ctx := context.Background()
	email = testsupport.RandomKey() + "@example.com"
	err := h.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', 2, $1, 'unused', $2)
		RETURNING id::text
	`, email, legalName).Scan(&identityID)
	if err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	acc, err := h.ledgerSvc.OpenAccount(ctx, identityID, ledger.AccountTypeWallet, "NGN", 0)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if fundMinor > 0 {
		if _, err := h.ledgerSvc.Credit(ctx, acc.ID, mustMoney(t, fundMinor), testsupport.RandomKey()); err != nil {
			t.Fatalf("fund account: %v", err)
		}
	}
	return identityID, email, acc.ID
}

func TestSendLowRiskSettlesImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, toAcc := h.newTierAccount(t, "Bob Recipient", 0)

	// Establish payee history directly via the ledger (not through Send,
	// which would itself be held as first-time) so the actual test send
	// below is genuinely "not first-time" and stays under the
	// large-amount threshold.
	if _, err := h.ledgerSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 1_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed payee history: %v", err)
	}

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeCompleted {
		t.Fatalf("outcome = %s, want completed", result.Outcome)
	}
	if result.PayeeResult != "match" {
		t.Fatalf("payee result = %s, want match", result.PayeeResult)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1_00+1_000_00 {
		t.Fatalf("recipient balance = %d, want 101000 (seed transfer + the low-risk send)", bal.Available.Minor)
	}
}

func TestSendFirstTimePayeeIsHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, toAcc := h.newTierAccount(t, "Bob Recipient", 0)

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeHeld {
		t.Fatalf("outcome = %s, want held (first-time payee)", result.Outcome)
	}
	found := false
	for _, r := range result.Reasons {
		if r == "first_time_payee" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want first_time_payee included", result.Reasons)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 0 {
		t.Fatalf("recipient balance = %d, want 0 (funds not yet moved)", bal.Available.Minor)
	}

	fromBal, err := h.ledgerSvc.GetBalance(ctx, fromAcc)
	if err != nil {
		t.Fatalf("get sender balance: %v", err)
	}
	if fromBal.Pending.Minor != 1_000_00 {
		t.Fatalf("sender pending = %d, want 100000 (hold reserved)", fromBal.Pending.Minor)
	}
}

func TestSendNoMatchWithoutConfirmBlocksAndMovesNoFunds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, _ := h.newTierAccount(t, "Bob Recipient", 0)

	_, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Someone Else Entirely", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if !errors.Is(err, coolingoff.ErrPayeeMismatchNotConfirmed) {
		t.Fatalf("err = %v, want ErrPayeeMismatchNotConfirmed", err)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, fromAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("sender balance = pending=%d available=%d, want 0/100000000 (nothing moved)", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestSendNoMatchWithConfirmIsHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, _ := h.newTierAccount(t, "Bob Recipient", 0)

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Someone Else Entirely", mustMoney(t, 1_000_00), testsupport.RandomKey(), true)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeHeld {
		t.Fatalf("outcome = %s, want held (mismatch confirmed but still high risk)", result.Outcome)
	}
}

func TestCancelReleasesHoldAndIsOwnershipChecked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	otherID, _, _ := h.newTierAccount(t, "Mallory Attacker", 0)
	_, toEmail, _ := h.newTierAccount(t, "Bob Recipient", 0)

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeHeld {
		t.Fatalf("outcome = %s, want held", result.Outcome)
	}

	if err := h.svc.Cancel(ctx, result.PendingID, otherID); !errors.Is(err, coolingoff.ErrNotPending) {
		t.Fatalf("cancel by non-owner err = %v, want ErrNotPending", err)
	}

	if err := h.svc.Cancel(ctx, result.PendingID, fromID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, fromAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("sender balance after cancel = pending=%d available=%d, want 0/100000000", bal.Pending.Minor, bal.Available.Minor)
	}

	if err := h.svc.Cancel(ctx, result.PendingID, fromID); !errors.Is(err, coolingoff.ErrNotPending) {
		t.Fatalf("second cancel err = %v, want ErrNotPending", err)
	}
}

func TestReleaseMaturedCapturesDueHoldsExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, toAcc := h.newTierAccount(t, "Bob Recipient", 0)

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeHeld {
		t.Fatalf("outcome = %s, want held", result.Outcome)
	}

	if _, err := h.pool.Exec(ctx, `UPDATE cooling_off_transfers SET release_at = now() - interval '1 minute' WHERE id = $1`, result.PendingID); err != nil {
		t.Fatalf("backdate release_at: %v", err)
	}

	if err := h.svc.ReleaseMatured(ctx); err != nil {
		t.Fatalf("release matured: %v", err)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get recipient balance: %v", err)
	}
	if bal.Available.Minor != 1_000_00 {
		t.Fatalf("recipient balance = %d, want 100000", bal.Available.Minor)
	}

	var status string
	if err := h.pool.QueryRow(ctx, `SELECT status FROM cooling_off_transfers WHERE id = $1`, result.PendingID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "completed" {
		t.Fatalf("status = %s, want completed", status)
	}

	// A second run must not double-move funds.
	if err := h.svc.ReleaseMatured(ctx); err != nil {
		t.Fatalf("release matured (second run): %v", err)
	}
	bal, err = h.ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get recipient balance again: %v", err)
	}
	if bal.Available.Minor != 1_000_00 {
		t.Fatalf("recipient balance after second run = %d, want unchanged 100000", bal.Available.Minor)
	}
}

func TestSend_BlockedByReadOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, _ := h.newTierAccount(t, "Bob Recipient", 0)

	if _, err := h.resilienceSvc.SetReadOnly(ctx, true, "drill", "test"); err != nil {
		t.Fatalf("SetReadOnly(true): %v", err)
	}
	t.Cleanup(func() {
		if _, err := h.resilienceSvc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	_, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if !errors.Is(err, resilience.ErrReadOnly) {
		t.Fatalf("send during read-only mode: got %v, want ErrReadOnly", err)
	}
}

// TestCancel_NotBlockedByReadOnly proves the initiation-vs-resolution rule
// (internal/resilience's package doc): read-only mode pauses new money
// movement, but cancelling an already-held transfer is de-risking and must
// keep working even mid-incident.
func TestCancel_NotBlockedByReadOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	fromID, _, fromAcc := h.newTierAccount(t, "Alice Sender", 1_000_000_00)
	_, toEmail, _ := h.newTierAccount(t, "Bob Recipient", 0)

	result, err := h.svc.Send(ctx, fromAcc, fromID, toEmail, "Bob Recipient", mustMoney(t, 1_000_00), testsupport.RandomKey(), false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Outcome != coolingoff.OutcomeHeld {
		t.Fatalf("outcome = %s, want held", result.Outcome)
	}

	if _, err := h.resilienceSvc.SetReadOnly(ctx, true, "drill", "test"); err != nil {
		t.Fatalf("SetReadOnly(true): %v", err)
	}
	t.Cleanup(func() {
		if _, err := h.resilienceSvc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	if err := h.svc.Cancel(ctx, result.PendingID, fromID); err != nil {
		t.Fatalf("cancel during read-only mode: %v", err)
	}
}
