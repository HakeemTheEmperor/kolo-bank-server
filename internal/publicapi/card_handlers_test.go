package publicapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func TestIssueListFreezeUnfreezeCardOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	identityID, token := newIndividualWithSession(t, env, "Card Holder", 1_000_000_00)

	var accountID string
	if err := env.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE owner_id = $1`, identityID).Scan(&accountID); err != nil {
		t.Fatalf("look up account: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/cards", token, testsupport.RandomKey(), map[string]any{
		"account_id": accountID, "card_type": "virtual",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var card struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decodeBody(t, rec, &card)
	if card.Status != "active" {
		t.Fatalf("status = %s, want active", card.Status)
	}

	listRec := doRequest(t, env, http.MethodGet, "/v1/me/cards", token, "", nil)
	var list []map[string]any
	decodeBody(t, listRec, &list)
	if len(list) != 1 {
		t.Fatalf("list = %+v, want 1 card", list)
	}

	freezeRec := doRequest(t, env, http.MethodPost, "/v1/me/cards/"+card.ID+"/freeze", token, "", nil)
	if freezeRec.Code != http.StatusNoContent {
		t.Fatalf("freeze status = %d, body = %s", freezeRec.Code, freezeRec.Body.String())
	}

	unfreezeRec := doRequest(t, env, http.MethodPost, "/v1/me/cards/"+card.ID+"/unfreeze", token, "", nil)
	if unfreezeRec.Code != http.StatusNoContent {
		t.Fatalf("unfreeze status = %d, body = %s", unfreezeRec.Code, unfreezeRec.Body.String())
	}
}

func TestCardRoutesAreOwnershipChecked(t *testing.T) {
	env := newTestEnv(t)
	ownerID, ownerToken := newIndividualWithSession(t, env, "Card Owner", 0)
	_, attackerToken := newIndividualWithSession(t, env, "Card Attacker", 0)

	var accountID string
	if err := env.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE owner_id = $1`, ownerID).Scan(&accountID); err != nil {
		t.Fatalf("look up account: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/cards", ownerToken, testsupport.RandomKey(), map[string]any{
		"account_id": accountID, "card_type": "virtual",
	})
	var card struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &card)

	freezeRec := doRequest(t, env, http.MethodPost, "/v1/me/cards/"+card.ID+"/freeze", attackerToken, "", nil)
	if freezeRec.Code != http.StatusNotFound {
		t.Fatalf("freeze-by-non-owner status = %d, want 404", freezeRec.Code)
	}
}

func TestCardAuthorizeSettleAndChargebackDisputeOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	identityID, token := newIndividualWithSession(t, env, "Card Spender", 1_000_000_00)

	var accountID string
	if err := env.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE owner_id = $1`, identityID).Scan(&accountID); err != nil {
		t.Fatalf("look up account: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/cards", token, testsupport.RandomKey(), map[string]any{
		"account_id": accountID, "card_type": "virtual",
	})
	var card struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &card)

	auth, err := env.cardsSvc.Authorize(context.Background(), card.ID, "Grocery Store", "5411", mustCardMoney(t, 10_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := env.cardsSvc.Settle(context.Background(), auth.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}

	disputeRec := doRequest(t, env, http.MethodPost, "/v1/me/disputes", token, "", map[string]any{
		"source_type": "card_authorization", "source_id": auth.ID, "reason": "did not authorize this purchase",
	})
	if disputeRec.Code != http.StatusCreated {
		t.Fatalf("create dispute status = %d, body = %s", disputeRec.Code, disputeRec.Body.String())
	}

	// A different customer must not be able to dispute this authorization.
	_, otherToken := newIndividualWithSession(t, env, "Someone Else", 0)
	otherDisputeRec := doRequest(t, env, http.MethodPost, "/v1/me/disputes", otherToken, "", map[string]any{
		"source_type": "card_authorization", "source_id": auth.ID, "reason": "not mine",
	})
	if otherDisputeRec.Code != http.StatusNotFound {
		t.Fatalf("dispute-by-non-owner status = %d, want 404", otherDisputeRec.Code)
	}

	if _, err := env.cardsSvc.Chargeback(context.Background(), auth.ID, testsupport.RandomKey()); err != nil {
		t.Fatalf("chargeback: %v", err)
	}
	bal, err := env.ledgerSvc.GetBalance(context.Background(), accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1_000_000_00 {
		t.Fatalf("available after chargeback = %d, want 100000000", bal.Available.Minor)
	}
}

func mustCardMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}
