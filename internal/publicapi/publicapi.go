package publicapi

import (
	"log/slog"
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

// Scopes recognized by the integration API. A merchant's API key must
// carry the relevant scope to call the corresponding route.
const (
	ScopeChargesWrite  = "charges:write"
	ScopeChargesRead   = "charges:read"
	ScopePayoutsWrite  = "payouts:write"
	ScopePayoutsRead   = "payouts:read"
	ScopeCheckoutWrite = "checkout:write"
)

type Deps struct {
	ApiKeys       *apikeys.Service
	Auth          *auth.Service
	Identity      *identity.Service
	Tokens        *tokens.Service
	Charges       *charges.Service
	Payouts       *payouts.Service
	Checkout      *checkout.Service
	Webhooks      *webhooks.Service
	Logger        *slog.Logger
	PublicBaseURL string
}

type api struct {
	deps Deps
}

// New builds the public integration API handler, meant to be mounted at
// /v1/ inside internal/httpserver's shared logging/tracing middleware
// (internal/httpserver/httpserver.go).
func New(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	a := &api{deps: deps}

	limiter := newRateLimiter(10, 20)
	withAPIKeyAuth := apiKeyAuth(deps.ApiKeys, limiter)
	withSessionAuth := sessionAuth(deps.Auth)

	mux := http.NewServeMux()

	// Integration API — API-key authenticated.
	mux.Handle("POST /v1/tokens", chain(withAPIKeyAuth, requireScope(ScopeChargesWrite), requireIdempotencyKey)(http.HandlerFunc(a.createToken)))

	mux.Handle("POST /v1/charges", chain(withAPIKeyAuth, requireScope(ScopeChargesWrite), requireIdempotencyKey)(http.HandlerFunc(a.createCharge)))
	mux.Handle("GET /v1/charges", chain(withAPIKeyAuth, requireScope(ScopeChargesRead))(http.HandlerFunc(a.listCharges)))
	mux.Handle("GET /v1/charges/{id}", chain(withAPIKeyAuth, requireScope(ScopeChargesRead))(http.HandlerFunc(a.getCharge)))

	mux.Handle("POST /v1/payouts", chain(withAPIKeyAuth, requireScope(ScopePayoutsWrite), requireIdempotencyKey)(http.HandlerFunc(a.createPayout)))
	mux.Handle("GET /v1/payouts", chain(withAPIKeyAuth, requireScope(ScopePayoutsRead))(http.HandlerFunc(a.listPayouts)))
	mux.Handle("GET /v1/payouts/{id}", chain(withAPIKeyAuth, requireScope(ScopePayoutsRead))(http.HandlerFunc(a.getPayout)))

	mux.Handle("POST /v1/checkout-sessions", chain(withAPIKeyAuth, requireScope(ScopeCheckoutWrite), requireIdempotencyKey)(http.HandlerFunc(a.createCheckoutSession)))
	mux.Handle("GET /v1/checkout-sessions/{id}", chain(withAPIKeyAuth, requireScope(ScopeCheckoutWrite))(http.HandlerFunc(a.getCheckoutSession)))

	// Merchant dashboard — session authenticated.
	mux.Handle("POST /v1/dashboard/api-keys", chain(withSessionAuth, requireIdempotencyKey)(http.HandlerFunc(a.createAPIKey)))
	mux.Handle("GET /v1/dashboard/api-keys", withSessionAuth(http.HandlerFunc(a.listAPIKeys)))
	mux.Handle("POST /v1/dashboard/api-keys/{id}/rotate", chain(withSessionAuth, requireIdempotencyKey)(http.HandlerFunc(a.rotateAPIKey)))
	mux.Handle("DELETE /v1/dashboard/api-keys/{id}", withSessionAuth(http.HandlerFunc(a.revokeAPIKey)))

	mux.Handle("POST /v1/dashboard/webhook-endpoints", chain(withSessionAuth, requireIdempotencyKey)(http.HandlerFunc(a.createWebhookEndpoint)))
	mux.Handle("GET /v1/dashboard/webhook-endpoints", withSessionAuth(http.HandlerFunc(a.listWebhookEndpoints)))
	mux.Handle("DELETE /v1/dashboard/webhook-endpoints/{id}", withSessionAuth(http.HandlerFunc(a.deactivateWebhookEndpoint)))

	return mux
}
