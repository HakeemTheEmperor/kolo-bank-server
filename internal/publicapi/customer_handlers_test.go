package publicapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
	}
}

// newIndividualWithSession creates an active individual identity with an
// open funded NGN account and a logged-in session — the customer-facing
// counterpart to newMerchantWithSession above.
func newIndividualWithSession(t *testing.T, env testEnv, legalName string, fundMinor int64) (identityID, sessionToken string) {
	t.Helper()
	ctx := context.Background()
	const password = "correct horse battery staple"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	email := testsupport.RandomKey() + "@example.com"
	err = env.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', 2, $1, $2, $3)
		RETURNING id::text
	`, email, hash, legalName).Scan(&identityID)
	if err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	acc, err := env.ledgerSvc.OpenAccount(ctx, identityID, ledger.AccountTypeWallet, "NGN", 0)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if fundMinor > 0 {
		if _, err := env.ledgerSvc.Credit(ctx, acc.ID, ledger.Money{Minor: fundMinor, Currency: "NGN"}, testsupport.RandomKey()); err != nil {
			t.Fatalf("fund account: %v", err)
		}
	}

	sessionToken, _, err = env.authSvc.Login(ctx, email, password, "test-device")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return identityID, sessionToken
}

func TestCreateTransferRequiresSession(t *testing.T) {
	env := newTestEnv(t)
	rec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", "", testsupport.RandomKey(), map[string]any{
		"recipient_email": "nobody@example.com", "recipient_name": "Nobody", "amount_minor": 1000, "currency": "NGN",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCreateTransferFirstTimePayeeIsHeld(t *testing.T) {
	env := newTestEnv(t)
	_, senderToken := newIndividualWithSession(t, env, "Alice Sender", 1_000_000_00)
	_, _ = newIndividualWithSession(t, env, "Bob Recipient", 0)

	// Look up the recipient's email directly since newIndividualWithSession
	// doesn't return it.
	var recipientEmail string
	if err := env.pool.QueryRow(context.Background(), `SELECT email FROM identities WHERE legal_name = 'Bob Recipient'`).Scan(&recipientEmail); err != nil {
		t.Fatalf("look up recipient email: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", senderToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": recipientEmail, "recipient_name": "Bob Recipient", "amount_minor": 1000_00, "currency": "NGN",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Outcome   string `json:"outcome"`
		PendingID string `json:"pending_id"`
	}
	decodeBody(t, rec, &body)
	if body.Outcome != "held" {
		t.Fatalf("outcome = %s, want held", body.Outcome)
	}
	if body.PendingID == "" {
		t.Fatal("expected a pending_id for a held transfer")
	}
}

func TestCreateTransferMismatchNotConfirmedIsBlocked(t *testing.T) {
	env := newTestEnv(t)
	_, senderToken := newIndividualWithSession(t, env, "Alice Sender", 1_000_000_00)
	_, _ = newIndividualWithSession(t, env, "Carol Recipient", 0)

	var recipientEmail string
	if err := env.pool.QueryRow(context.Background(), `SELECT email FROM identities WHERE legal_name = 'Carol Recipient'`).Scan(&recipientEmail); err != nil {
		t.Fatalf("look up recipient email: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", senderToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": recipientEmail, "recipient_name": "A Totally Different Person", "amount_minor": 1000, "currency": "NGN",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestCancelAndListPendingTransfer(t *testing.T) {
	env := newTestEnv(t)
	_, senderToken := newIndividualWithSession(t, env, "Alice Sender", 1_000_000_00)
	_, _ = newIndividualWithSession(t, env, "Dave Recipient", 0)

	var recipientEmail string
	if err := env.pool.QueryRow(context.Background(), `SELECT email FROM identities WHERE legal_name = 'Dave Recipient'`).Scan(&recipientEmail); err != nil {
		t.Fatalf("look up recipient email: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", senderToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": recipientEmail, "recipient_name": "Dave Recipient", "amount_minor": 1000_00, "currency": "NGN",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create transfer status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PendingID string `json:"pending_id"`
	}
	decodeBody(t, rec, &body)

	listRec := doRequest(t, env, http.MethodGet, "/v1/me/transfers/pending", senderToken, "", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d", listRec.Code)
	}
	var pending []map[string]any
	decodeBody(t, listRec, &pending)
	if len(pending) != 1 {
		t.Fatalf("pending list = %+v, want exactly 1", pending)
	}

	cancelRec := doRequest(t, env, http.MethodPost, "/v1/me/transfers/"+body.PendingID+"/cancel", senderToken, "", nil)
	if cancelRec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}

	listRec = doRequest(t, env, http.MethodGet, "/v1/me/transfers/pending", senderToken, "", nil)
	decodeBody(t, listRec, &pending)
	if len(pending) != 0 {
		t.Fatalf("pending list after cancel = %+v, want empty", pending)
	}
}

func TestGrantListRevokeAuthorizationOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	_, customerToken := newIndividualWithSession(t, env, "Alice Customer", 0)
	merchantID, _ := newMerchantWithSession(t, env)

	rec := doRequest(t, env, http.MethodPost, "/v1/me/authorize/"+merchantID, customerToken, "", map[string]any{
		"scopes": []string{"read_balance"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var granted struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &granted)

	listRec := doRequest(t, env, http.MethodGet, "/v1/me/authorizations", customerToken, "", nil)
	var list []map[string]any
	decodeBody(t, listRec, &list)
	if len(list) != 1 {
		t.Fatalf("authorizations = %+v, want exactly 1", list)
	}

	revokeRec := doRequest(t, env, http.MethodDelete, "/v1/me/authorizations/"+granted.ID, customerToken, "", nil)
	if revokeRec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", revokeRec.Code, revokeRec.Body.String())
	}
}

func TestCreateDisputeRejectsUnownedTransfer(t *testing.T) {
	env := newTestEnv(t)
	_, customerToken := newIndividualWithSession(t, env, "Alice Customer", 0)

	rec := doRequest(t, env, http.MethodPost, "/v1/me/disputes", customerToken, "", map[string]any{
		"source_type": "external_transfer", "source_id": testsupport.RandomUUID(), "reason": "never received",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 (transfer doesn't belong to caller)", rec.Code, rec.Body.String())
	}
}

// TestCreateDisputeOnCoolingOffTransfer covers a path that was silently
// broken from Phase 8 through the start of Phase 9: this handler already
// accepted source_type "cooling_off_transfer", but the disputes table's
// CHECK constraint (00036_create_dispute_tables.sql) didn't allow it until
// 00043_widen_dispute_source_types.sql. No prior test exercised it.
func TestCreateDisputeOnCoolingOffTransfer(t *testing.T) {
	env := newTestEnv(t)
	_, senderToken := newIndividualWithSession(t, env, "Alice Sender", 1_000_000_00)
	_, _ = newIndividualWithSession(t, env, "Eve Recipient", 0)

	var recipientEmail string
	if err := env.pool.QueryRow(context.Background(), `SELECT email FROM identities WHERE legal_name = 'Eve Recipient'`).Scan(&recipientEmail); err != nil {
		t.Fatalf("look up recipient email: %v", err)
	}

	transferRec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", senderToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": recipientEmail, "recipient_name": "Eve Recipient", "amount_minor": 1000_00, "currency": "NGN",
	})
	if transferRec.Code != http.StatusCreated {
		t.Fatalf("create transfer status = %d, body = %s", transferRec.Code, transferRec.Body.String())
	}
	var transferBody struct {
		PendingID string `json:"pending_id"`
	}
	decodeBody(t, transferRec, &transferBody)

	disputeRec := doRequest(t, env, http.MethodPost, "/v1/me/disputes", senderToken, "", map[string]any{
		"source_type": "cooling_off_transfer", "source_id": transferBody.PendingID, "reason": "changed my mind",
	})
	if disputeRec.Code != http.StatusCreated {
		t.Fatalf("create dispute status = %d, body = %s", disputeRec.Code, disputeRec.Body.String())
	}
}

func TestRecoveryInitiateAndCompletePublic(t *testing.T) {
	env := newTestEnv(t)
	_, _ = newIndividualWithSession(t, env, "Recovery Test User", 0)

	var email string
	if err := env.pool.QueryRow(context.Background(), `SELECT email FROM identities WHERE legal_name = 'Recovery Test User'`).Scan(&email); err != nil {
		t.Fatalf("look up email: %v", err)
	}

	rec := doRequest(t, env, http.MethodPost, "/v1/recovery/initiate", "", "", map[string]any{
		"email": email, "device_fingerprint": "recovery-device", "legal_name": "Recovery Test User", "address": "1 Main St",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("initiate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &body)
	if body.ID == "" {
		t.Fatal("expected a request id")
	}

	// Not yet eligible.
	completeRec := doRequest(t, env, http.MethodPost, "/v1/recovery/"+body.ID+"/complete", "", "", map[string]any{
		"new_password": "a brand new passphrase",
	})
	if completeRec.Code != http.StatusConflict {
		t.Fatalf("complete-too-early status = %d, want 409", completeRec.Code)
	}

	if _, err := env.pool.Exec(context.Background(), `UPDATE account_recovery_requests SET eligible_at = now() - interval '1 minute' WHERE id = $1`, body.ID); err != nil {
		t.Fatalf("backdate eligible_at: %v", err)
	}

	completeRec = doRequest(t, env, http.MethodPost, "/v1/recovery/"+body.ID+"/complete", "", "", map[string]any{
		"new_password": "a brand new passphrase",
	})
	if completeRec.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d, body = %s", completeRec.Code, completeRec.Body.String())
	}
}
