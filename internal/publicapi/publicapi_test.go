package publicapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/publicapi"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

type testEnv struct {
	handler     http.Handler
	pool        *pgxpool.Pool
	ledgerSvc   *ledger.Service
	externalSvc *externalpayments.Service
	apiKeysSvc  *apikeys.Service
	authSvc     *auth.Service
}

func newTestEnv(t *testing.T) testEnv {
	t.Helper()
	pool := testsupport.RequireTestPool(t)

	identitySvc := identity.NewService(pool)
	ledgerSvc := ledger.NewService(pool)
	registry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(pool, ledgerSvc, registry, nil)
	kp := secrets.NewLocalKeyProvider()

	deps := publicapi.Deps{
		ApiKeys:       apikeys.NewService(pool),
		Auth:          auth.NewService(pool, identitySvc, kp),
		Identity:      identitySvc,
		Tokens:        tokens.NewService(pool),
		Charges:       charges.NewService(pool, tokens.NewService(pool), externalSvc),
		Payouts:       payouts.NewService(pool, externalSvc, registry),
		Checkout:      checkout.NewService(pool),
		Webhooks:      webhooks.NewService(pool, kp),
		PublicBaseURL: "https://api.kolobank.example",
	}

	return testEnv{
		handler:     publicapi.New(deps),
		pool:        pool,
		ledgerSvc:   ledgerSvc,
		externalSvc: externalSvc,
		apiKeysSvc:  deps.ApiKeys,
		authSvc:     deps.Auth,
	}
}

// newMerchantWithKey creates an active business identity, an open NGN
// settlement account, and a live API key with the given scopes.
func newMerchantWithKey(t *testing.T, env testEnv, scopes ...string) (merchantID, rawKey string) {
	t.Helper()
	ctx := context.Background()

	err := env.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&merchantID)
	if err != nil {
		t.Fatalf("insert merchant: %v", err)
	}
	if _, err := env.ledgerSvc.OpenAccount(ctx, merchantID, ledger.AccountTypeCurrent, "NGN", 0); err != nil {
		t.Fatalf("open account: %v", err)
	}

	rawKey, _, err = env.apiKeysSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "Test key", scopes)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return merchantID, rawKey
}

// newMerchantWithSession creates an active business identity with a real
// password and returns a bearer session token from Login, for dashboard
// routes.
func newMerchantWithSession(t *testing.T, env testEnv) (merchantID, sessionToken string) {
	t.Helper()
	ctx := context.Background()
	const password = "correct horse battery staple"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	email := testsupport.RandomKey() + "@example.com"
	err = env.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, $2, 'Test Merchant')
		RETURNING id::text
	`, email, hash).Scan(&merchantID)
	if err != nil {
		t.Fatalf("insert merchant: %v", err)
	}

	sessionToken, _, err = env.authSvc.Login(ctx, email, password, "test-device")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return merchantID, sessionToken
}

func doRequest(t *testing.T, env testEnv, method, path, bearer, idempotencyKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestFullChargeFlowOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesWrite, publicapi.ScopeChargesRead)

	tokenRec := doRequest(t, env, http.MethodPost, "/v1/tokens", rawKey, testsupport.RandomKey(), map[string]any{
		"card_number": "4242424242424242",
	})
	if tokenRec.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, body = %s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	chargeRec := doRequest(t, env, http.MethodPost, "/v1/charges", rawKey, testsupport.RandomKey(), map[string]any{
		"token_id": tokenResp.ID, "amount_minor": 5_000_00, "currency": "NGN",
	})
	if chargeRec.Code != http.StatusCreated {
		t.Fatalf("create charge status = %d, body = %s", chargeRec.Code, chargeRec.Body.String())
	}
	var chargeResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(chargeRec.Body.Bytes(), &chargeResp); err != nil {
		t.Fatalf("decode charge response: %v", err)
	}
	if chargeResp.Status != "pending" {
		t.Fatalf("charge status = %s, want pending", chargeResp.Status)
	}

	if err := env.externalSvc.ProcessPending(context.Background()); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	getRec := doRequest(t, env, http.MethodGet, "/v1/charges/"+chargeResp.ID, rawKey, "", nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get charge status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &chargeResp); err != nil {
		t.Fatalf("decode get charge response: %v", err)
	}
	if chargeResp.Status != "succeeded" {
		t.Fatalf("charge status after processing = %s, want succeeded", chargeResp.Status)
	}
}

func TestMissingAPIKeyRejected(t *testing.T) {
	env := newTestEnv(t)
	rec := doRequest(t, env, http.MethodGet, "/v1/charges", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestInvalidAPIKeyRejected(t *testing.T) {
	env := newTestEnv(t)
	rec := doRequest(t, env, http.MethodGet, "/v1/charges", "kolo_test_bogus", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMissingIdempotencyKeyRejected(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesWrite)

	rec := doRequest(t, env, http.MethodPost, "/v1/tokens", rawKey, "", map[string]any{"card_number": "4242424242424242"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestIdempotencyKeyReuseReturnsSameResource(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesWrite)
	idemKey := testsupport.RandomKey()

	first := doRequest(t, env, http.MethodPost, "/v1/tokens", rawKey, idemKey, map[string]any{"card_number": "4242424242424242"})
	second := doRequest(t, env, http.MethodPost, "/v1/tokens", rawKey, idemKey, map[string]any{"card_number": "4242424242424242"})

	var firstResp, secondResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)
	_ = json.Unmarshal(second.Body.Bytes(), &secondResp)
	if firstResp.ID != secondResp.ID {
		t.Fatalf("retried request produced a different resource: %s != %s", firstResp.ID, secondResp.ID)
	}
}

func TestMissingScopeRejected(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesRead) // no charges:write

	rec := doRequest(t, env, http.MethodPost, "/v1/tokens", rawKey, testsupport.RandomKey(), map[string]any{"card_number": "4242424242424242"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRateLimitReturns429AfterBurst(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesRead)

	var sawRateLimited bool
	for i := 0; i < 30; i++ {
		rec := doRequest(t, env, http.MethodGet, "/v1/charges", rawKey, "", nil)
		if rec.Code == http.StatusTooManyRequests {
			sawRateLimited = true
			break
		}
	}
	if !sawRateLimited {
		t.Fatal("expected at least one request in a rapid burst to be rate limited")
	}
}

func TestDashboardRejectsAPIKey(t *testing.T) {
	env := newTestEnv(t)
	_, rawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesWrite)

	rec := doRequest(t, env, http.MethodGet, "/v1/dashboard/api-keys", rawKey, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (API key must not authenticate dashboard routes)", rec.Code)
	}
}

func TestIntegrationRouteRejectsSessionToken(t *testing.T) {
	env := newTestEnv(t)
	_, sessionToken := newMerchantWithSession(t, env)

	rec := doRequest(t, env, http.MethodGet, "/v1/charges", sessionToken, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (session token must not authenticate integration routes)", rec.Code)
	}
}

func TestDashboardCreateAndRotateAPIKey(t *testing.T) {
	env := newTestEnv(t)
	_, sessionToken := newMerchantWithSession(t, env)

	createRec := doRequest(t, env, http.MethodPost, "/v1/dashboard/api-keys", sessionToken, testsupport.RandomKey(), map[string]any{
		"mode": "sandbox", "name": "My key", "scopes": []string{"charges:write"},
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create api key status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Key == "" {
		t.Fatal("expected the raw key to be returned on creation")
	}

	rotateRec := doRequest(t, env, http.MethodPost, "/v1/dashboard/api-keys/"+created.ID+"/rotate", sessionToken, testsupport.RandomKey(), nil)
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rotateRec.Body.Bytes(), &rotated)
	if rotated.Key == created.Key {
		t.Fatal("expected rotation to produce a different raw key")
	}
}

func TestDashboardCannotActOnAnotherMerchantsKey(t *testing.T) {
	env := newTestEnv(t)
	_, otherRawKey := newMerchantWithKey(t, env, publicapi.ScopeChargesWrite)
	// Look up the other merchant's key id directly for this ownership test.
	var otherKeyID string
	if err := env.pool.QueryRow(context.Background(), `SELECT id::text FROM api_keys LIMIT 1`).Scan(&otherKeyID); err != nil {
		t.Fatalf("query other key id: %v", err)
	}
	_ = otherRawKey

	_, sessionToken := newMerchantWithSession(t, env)

	rec := doRequest(t, env, http.MethodDelete, "/v1/dashboard/api-keys/"+otherKeyID, sessionToken, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (must not be able to revoke another merchant's key)", rec.Code)
	}
}
