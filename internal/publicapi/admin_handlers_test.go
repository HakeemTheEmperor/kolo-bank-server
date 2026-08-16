package publicapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/publicapi"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// testAdminKey matches the AdminAPIKey newTestEnv configures (publicapi_test.go).
const testAdminKey = "test-admin-key"

func TestAdminAuth_RejectsMissingOrWrongToken(t *testing.T) {
	env := newTestEnv(t)

	rec := doRequest(t, env, http.MethodGet, "/v1/admin/resilience", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}

	rec = doRequest(t, env, http.MethodGet, "/v1/admin/resilience", "wrong-key", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}
}

func TestAdminAuth_RejectsWhenNoAdminKeyConfigured(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	handler := publicapi.New(publicapi.Deps{
		Resilience:  resilience.NewService(pool),
		AdminAPIKey: "",
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/resilience", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with unconfigured admin key = %d, want 401 (unset must mean unreachable, not open)", rec.Code)
	}
}

func TestSetKillSwitch_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	scopeValue := "test-feature-" + testsupport.RandomKey()

	rec := doRequest(t, env, http.MethodPut, "/v1/admin/resilience/kill-switches/feature/"+scopeValue, testAdminKey, "", map[string]any{
		"enabled": false, "reason": "incident", "updated_by": "ops@kolo",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set kill switch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var ks struct {
		ScopeType  string `json:"scope_type"`
		ScopeValue string `json:"scope_value"`
		Enabled    bool   `json:"enabled"`
		Reason     string `json:"reason"`
	}
	decodeBody(t, rec, &ks)
	if ks.ScopeType != "feature" || ks.ScopeValue != scopeValue || ks.Enabled || ks.Reason != "incident" {
		t.Fatalf("kill switch = %+v, want feature/%s disabled/incident", ks, scopeValue)
	}

	listRec := doRequest(t, env, http.MethodGet, "/v1/admin/resilience/kill-switches", testAdminKey, "", nil)
	var listBody struct {
		KillSwitches []struct {
			ScopeValue string `json:"scope_value"`
			Enabled    bool   `json:"enabled"`
		} `json:"kill_switches"`
	}
	decodeBody(t, listRec, &listBody)
	var found bool
	for _, item := range listBody.KillSwitches {
		if item.ScopeValue == scopeValue {
			found = true
			if item.Enabled {
				t.Fatalf("listed switch enabled = true, want false")
			}
		}
	}
	if !found {
		t.Fatalf("list did not include the switch just created (scope_value=%s)", scopeValue)
	}

	stateRec := doRequest(t, env, http.MethodGet, "/v1/admin/resilience", testAdminKey, "", nil)
	var state struct {
		KillSwitchesTripped []struct {
			ScopeValue string `json:"scope_value"`
		} `json:"kill_switches_tripped"`
	}
	decodeBody(t, stateRec, &state)
	found = false
	for _, item := range state.KillSwitchesTripped {
		if item.ScopeValue == scopeValue {
			found = true
		}
	}
	if !found {
		t.Fatalf("resilience state did not list the tripped switch (scope_value=%s)", scopeValue)
	}
}

func TestSetKillSwitch_RejectsInvalidScopeType(t *testing.T) {
	env := newTestEnv(t)

	rec := doRequest(t, env, http.MethodPut, "/v1/admin/resilience/kill-switches/bogus/whatever", testAdminKey, "", map[string]any{
		"enabled": false,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope type status = %d, want 400", rec.Code)
	}
}

func TestEnterExitReadOnly_RoundTrip(t *testing.T) {
	env := newTestEnv(t)

	enterRec := doRequest(t, env, http.MethodPost, "/v1/admin/resilience/read-only/enter", testAdminKey, "", map[string]any{
		"reason": "drill", "updated_by": "ops@kolo",
	})
	if enterRec.Code != http.StatusOK {
		t.Fatalf("enter read-only status = %d, body = %s", enterRec.Code, enterRec.Body.String())
	}
	t.Cleanup(func() {
		if _, err := env.resilienceSvc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	var entered struct {
		ReadOnly bool   `json:"read_only"`
		Reason   string `json:"reason"`
	}
	decodeBody(t, enterRec, &entered)
	if !entered.ReadOnly || entered.Reason != "drill" {
		t.Fatalf("entered = %+v, want read_only=true reason=drill", entered)
	}

	stateRec := doRequest(t, env, http.MethodGet, "/v1/admin/resilience", testAdminKey, "", nil)
	var state struct {
		ReadOnly bool `json:"read_only"`
	}
	decodeBody(t, stateRec, &state)
	if !state.ReadOnly {
		t.Fatalf("state after enter = %+v, want read_only=true", state)
	}

	exitRec := doRequest(t, env, http.MethodPost, "/v1/admin/resilience/read-only/exit", testAdminKey, "", map[string]any{
		"updated_by": "ops@kolo",
	})
	if exitRec.Code != http.StatusOK {
		t.Fatalf("exit read-only status = %d, body = %s", exitRec.Code, exitRec.Body.String())
	}
	var exited struct {
		ReadOnly bool `json:"read_only"`
	}
	decodeBody(t, exitRec, &exited)
	if exited.ReadOnly {
		t.Fatalf("exited.ReadOnly = true, want false")
	}
}

// TestEnterReadOnly_BlocksSubsequentTransfer is the end-to-end proof of the
// exit criterion "the system enters and exits read-only mode cleanly": a
// real transfer through the full handler stack is refused while the admin
// surface has the system in read-only mode, and succeeds again once it
// exits.
func TestEnterReadOnly_BlocksSubsequentTransfer(t *testing.T) {
	env := newTestEnv(t)
	fromID, fromToken := newIndividualWithSession(t, env, "Read Only Sender", 1_000_000_00)

	toEmail := testsupport.RandomKey() + "@example.com"
	var toID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', 2, $1, 'unused', 'Read Only Recipient')
		RETURNING id::text
	`, toEmail).Scan(&toID); err != nil {
		t.Fatalf("insert recipient identity: %v", err)
	}
	if _, err := env.ledgerSvc.OpenAccount(context.Background(), toID, ledger.AccountTypeWallet, "NGN", 0); err != nil {
		t.Fatalf("open recipient account: %v", err)
	}
	_ = fromID

	enterRec := doRequest(t, env, http.MethodPost, "/v1/admin/resilience/read-only/enter", testAdminKey, "", map[string]any{
		"reason": "drill", "updated_by": "ops@kolo",
	})
	if enterRec.Code != http.StatusOK {
		t.Fatalf("enter read-only status = %d", enterRec.Code)
	}
	t.Cleanup(func() {
		if _, err := env.resilienceSvc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	transferRec := doRequest(t, env, http.MethodPost, "/v1/me/transfers", fromToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": toEmail, "recipient_name": "Read Only Recipient", "amount_minor": 1_000_00, "currency": "NGN",
	})
	if transferRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("transfer during read-only mode status = %d, body = %s, want 503", transferRec.Code, transferRec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeBody(t, transferRec, &errBody)
	if errBody.Error.Code != "read_only_mode" {
		t.Fatalf("error code = %q, want read_only_mode", errBody.Error.Code)
	}

	exitRec := doRequest(t, env, http.MethodPost, "/v1/admin/resilience/read-only/exit", testAdminKey, "", map[string]any{"updated_by": "ops@kolo"})
	if exitRec.Code != http.StatusOK {
		t.Fatalf("exit read-only status = %d", exitRec.Code)
	}

	transferRec = doRequest(t, env, http.MethodPost, "/v1/me/transfers", fromToken, testsupport.RandomKey(), map[string]any{
		"recipient_email": toEmail, "recipient_name": "Read Only Recipient", "amount_minor": 1_000_00, "currency": "NGN",
	})
	if transferRec.Code != http.StatusCreated {
		t.Fatalf("transfer after exiting read-only status = %d, body = %s, want 201", transferRec.Code, transferRec.Body.String())
	}
}
