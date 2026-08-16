package publicapi

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/cards"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/consent"
	"github.com/toluwalase/kolo-bank-server/internal/coolingoff"
	"github.com/toluwalase/kolo-bank-server/internal/disputes"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/recovery"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
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
	CoolingOff    *coolingoff.Service
	Consent       *consent.Service
	Disputes      *disputes.Service
	Recovery      *recovery.Service
	Cards         *cards.Service
	Resilience    *resilience.Service
	AdminAPIKey   string
	Pool          *pgxpool.Pool
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

	// Customer safeguards (docs/banking-backend-spec.md §5) — session
	// authenticated via the same sessionAuth any identity (not just
	// merchants) logs into.
	mux.Handle("POST /v1/me/transfers", chain(withSessionAuth, requireIdempotencyKey)(http.HandlerFunc(a.createTransfer)))
	mux.Handle("GET /v1/me/transfers/pending", withSessionAuth(http.HandlerFunc(a.listPendingTransfers)))
	mux.Handle("POST /v1/me/transfers/{id}/cancel", withSessionAuth(http.HandlerFunc(a.cancelTransfer)))

	mux.Handle("POST /v1/me/authorize/{merchantID}", withSessionAuth(http.HandlerFunc(a.grantAuthorization)))
	mux.Handle("GET /v1/me/authorizations", withSessionAuth(http.HandlerFunc(a.listAuthorizations)))
	mux.Handle("DELETE /v1/me/authorizations/{id}", withSessionAuth(http.HandlerFunc(a.revokeAuthorization)))

	mux.Handle("POST /v1/me/disputes", withSessionAuth(http.HandlerFunc(a.createDispute)))
	mux.Handle("GET /v1/me/disputes", withSessionAuth(http.HandlerFunc(a.listDisputes)))
	mux.Handle("GET /v1/me/disputes/{id}", withSessionAuth(http.HandlerFunc(a.getDispute)))

	// Cards (docs/banking-backend-spec.md §3.5) — issuing and controls are
	// customer actions; authorize/settle/void stay service-layer only (see
	// card_handlers.go / the plan doc) since no HTTP caller of ours would
	// play the role of the card network.
	mux.Handle("POST /v1/me/cards", chain(withSessionAuth, requireIdempotencyKey)(http.HandlerFunc(a.issueCard)))
	mux.Handle("GET /v1/me/cards", withSessionAuth(http.HandlerFunc(a.listCards)))
	mux.Handle("POST /v1/me/cards/{id}/freeze", withSessionAuth(http.HandlerFunc(a.freezeCard)))
	mux.Handle("POST /v1/me/cards/{id}/unfreeze", withSessionAuth(http.HandlerFunc(a.unfreezeCard)))
	mux.Handle("PUT /v1/me/cards/{id}/limits", withSessionAuth(http.HandlerFunc(a.setCardLimits)))
	mux.Handle("GET /v1/me/cards/{id}/authorizations", withSessionAuth(http.HandlerFunc(a.listCardAuthorizations)))
	mux.Handle("POST /v1/me/cards/authorizations/{id}/3ds", withSessionAuth(http.HandlerFunc(a.completeCard3DS)))

	// Resilience admin surface (docs/banking-backend-spec.md §Phase 10) —
	// kill switches and read-only mode, authenticated by a single static
	// admin key rather than the session/API-key schemes above, since there
	// is no admin-user system in this project (see adminAuth's doc
	// comment). Not exposed to merchants or customers at all.
	withAdminAuth := adminAuth(deps.AdminAPIKey)
	mux.Handle("GET /v1/admin/resilience", withAdminAuth(http.HandlerFunc(a.getResilienceState)))
	mux.Handle("GET /v1/admin/resilience/kill-switches", withAdminAuth(http.HandlerFunc(a.listKillSwitches)))
	mux.Handle("PUT /v1/admin/resilience/kill-switches/{scopeType}/{scopeValue}", withAdminAuth(http.HandlerFunc(a.setKillSwitch)))
	mux.Handle("POST /v1/admin/resilience/read-only/enter", withAdminAuth(http.HandlerFunc(a.enterReadOnly)))
	mux.Handle("POST /v1/admin/resilience/read-only/exit", withAdminAuth(http.HandlerFunc(a.exitReadOnly)))

	// Account recovery — deliberately public; see recovery_handlers.go.
	recoveryLimiter := newRateLimiter(0.5, 5)
	withIPLimit := ipRateLimit(recoveryLimiter)
	mux.Handle("POST /v1/recovery/initiate", withIPLimit(http.HandlerFunc(a.initiateRecovery)))
	mux.Handle("POST /v1/recovery/{id}/complete", withIPLimit(http.HandlerFunc(a.completeRecovery)))

	return mux
}
