package cards_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/cards"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
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
	pool      *pgxpool.Pool
	ledgerSvc *ledger.Service
	svc       *cards.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	pool := testsupport.RequireTestPool(t)
	ledgerSvc := ledger.NewService(pool)
	return harness{pool: pool, ledgerSvc: ledgerSvc, svc: cards.NewService(pool, ledgerSvc)}
}

func (h harness) newFundedAccount(t *testing.T, fundMinor int64) (identityID, accountID string) {
	t.Helper()
	ctx := context.Background()
	err := h.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, 'unused', 'Test Cardholder')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&identityID)
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
	return identityID, acc.ID
}

func TestIssueVirtualAndPhysical(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 0)

	virtual, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue virtual: %v", err)
	}
	if virtual.CardType != cards.CardTypeVirtual || virtual.Status != cards.StatusActive {
		t.Fatalf("virtual card = %+v, want type=virtual status=active", virtual)
	}

	physical, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypePhysical)
	if err != nil {
		t.Fatalf("issue physical: %v", err)
	}
	if physical.CardType != cards.CardTypePhysical {
		t.Fatalf("physical card type = %s, want physical", physical.CardType)
	}

	list, err := h.svc.ListByIdentity(ctx, identityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want 2 cards", list)
	}
}

func TestFrozenCardDeclinesAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.Freeze(ctx, card.ID); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Coffee Shop", "5812", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusDeclined || auth.DeclineReason != "card_inactive" {
		t.Fatalf("auth = %+v, want declined/card_inactive", auth)
	}
}

func TestMCCBlockDeclines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.SetLimits(ctx, card.ID, 500_000_00, 200_000_00, []string{"7995"}); err != nil {
		t.Fatalf("set limits: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Casino Royale", "7995", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusDeclined || auth.DeclineReason != "mcc_blocked" {
		t.Fatalf("auth = %+v, want declined/mcc_blocked", auth)
	}
}

func TestPerTransactionLimitDeclines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.SetLimits(ctx, card.ID, 500_000_00, 50_000_00, nil); err != nil {
		t.Fatalf("set limits: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Electronics Store", "5732", mustMoney(t, 60_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusDeclined || auth.DeclineReason != "limit_exceeded" {
		t.Fatalf("auth = %+v, want declined/limit_exceeded", auth)
	}
}

func TestDailyLimitDeclines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.SetLimits(ctx, card.ID, 50_000_00, 40_000_00, nil); err != nil {
		t.Fatalf("set limits: %v", err)
	}

	first, err := h.svc.Authorize(ctx, card.ID, "Grocery Store", "5411", mustMoney(t, 30_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize first: %v", err)
	}
	if first.Status != cards.AuthStatusApproved {
		t.Fatalf("first auth = %+v, want approved", first)
	}

	second, err := h.svc.Authorize(ctx, card.ID, "Grocery Store", "5411", mustMoney(t, 30_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize second: %v", err)
	}
	if second.Status != cards.AuthStatusDeclined || second.DeclineReason != "daily_limit_exceeded" {
		t.Fatalf("second auth = %+v, want declined/daily_limit_exceeded", second)
	}
}

func TestNetworkDeclineMarker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Merchant NETWORKDECLINE Test", "5411", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusDeclined || auth.DeclineReason != "network_declined" {
		t.Fatalf("auth = %+v, want declined/network_declined", auth)
	}
}

func TestLargeAmountRequires3DSAndNoHoldUntilCompleted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 10_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.SetLimits(ctx, card.ID, 5_000_000_00, 5_000_000_00, nil); err != nil {
		t.Fatalf("set limits: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Big Ticket Electronics", "5732", mustMoney(t, 200_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusRequires3DS {
		t.Fatalf("auth = %+v, want requires_3ds", auth)
	}
	if auth.HoldID != nil {
		t.Fatal("expected no hold placed before 3ds completes")
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 {
		t.Fatalf("pending = %d, want 0 (no hold yet)", bal.Pending.Minor)
	}

	code, ok := cards.PeekThreeDSCode(auth.ID)
	if !ok {
		t.Fatal("expected a stashed 3ds code")
	}

	completed, err := h.svc.Complete3DS(ctx, auth.ID, code)
	if err != nil {
		t.Fatalf("complete 3ds: %v", err)
	}
	if completed.Status != cards.AuthStatusApproved || completed.HoldID == nil {
		t.Fatalf("completed = %+v, want approved with a hold", completed)
	}

	bal, err = h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance after 3ds: %v", err)
	}
	if bal.Pending.Minor != 200_000_00 {
		t.Fatalf("pending after 3ds = %d, want 20000000", bal.Pending.Minor)
	}
}

func TestComplete3DSWrongCodeDeclines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 10_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := h.svc.SetLimits(ctx, card.ID, 5_000_000_00, 5_000_000_00, nil); err != nil {
		t.Fatalf("set limits: %v", err)
	}

	auth, err := h.svc.Authorize(ctx, card.ID, "Big Ticket Electronics", "5732", mustMoney(t, 200_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	declined, err := h.svc.Complete3DS(ctx, auth.ID, "000000")
	if err != nil {
		t.Fatalf("complete 3ds with wrong code: %v", err)
	}
	if declined.Status != cards.AuthStatusDeclined || declined.DeclineReason != "3ds_failed" {
		t.Fatalf("declined = %+v, want declined/3ds_failed", declined)
	}
}

func TestSettleCapturesHoldIntoPostedTransaction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	auth, err := h.svc.Authorize(ctx, card.ID, "Grocery Store", "5411", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if auth.Status != cards.AuthStatusApproved {
		t.Fatalf("auth = %+v, want approved", auth)
	}

	settled, err := h.svc.Settle(ctx, auth.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled.Status != cards.AuthStatusSettled || settled.LedgerTransactionID == nil {
		t.Fatalf("settled = %+v, want settled with a ledger transaction", settled)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 990_000_00 {
		t.Fatalf("balance after settle: pending=%d available=%d, want 0/99000000", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestVoidReleasesUnsettledHold(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	auth, err := h.svc.Authorize(ctx, card.ID, "Grocery Store", "5411", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	if err := h.svc.Void(ctx, auth.ID); err != nil {
		t.Fatalf("void: %v", err)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("balance after void: pending=%d available=%d, want 0/100000000", bal.Pending.Minor, bal.Available.Minor)
	}

	if err := h.svc.Void(ctx, auth.ID); !errors.Is(err, cards.ErrNotAuthorized) {
		t.Fatalf("second void err = %v, want ErrNotAuthorized", err)
	}
}

func TestChargebackReversesSettledTransactionAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	identityID, accountID := h.newFundedAccount(t, 1_000_000_00)

	card, err := h.svc.Issue(ctx, identityID, accountID, cards.CardTypeVirtual)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	auth, err := h.svc.Authorize(ctx, card.ID, "Grocery Store", "5411", mustMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := h.svc.Settle(ctx, auth.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}

	key := testsupport.RandomKey()
	first, err := h.svc.Chargeback(ctx, auth.ID, key)
	if err != nil {
		t.Fatalf("chargeback: %v", err)
	}
	if first.Status != cards.AuthStatusChargedBack {
		t.Fatalf("status = %s, want charged_back", first.Status)
	}

	bal, err := h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("available after chargeback = %d, want 100000000 (fully reversed)", bal.Available.Minor)
	}

	// A second chargeback call (e.g. a retried request) must not error or
	// double-reverse.
	second, err := h.svc.Chargeback(ctx, auth.ID, key)
	if err != nil {
		t.Fatalf("second chargeback: %v", err)
	}
	if second.Status != cards.AuthStatusChargedBack {
		t.Fatalf("second status = %s, want charged_back", second.Status)
	}
	bal, err = h.ledgerSvc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance again: %v", err)
	}
	if bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("available after second chargeback = %d, want unchanged 100000000", bal.Available.Minor)
	}
}
