// Package publicapi is the public integration API's HTTP layer
// (docs/banking-backend-spec.md §3.6) — the first real HTTP surface in the
// project; Phases 1-4 were deliberately service-layer only. It wires
// together apikeys, tokens, charges, payouts, checkout, and webhooks
// behind two auth schemes: API keys for integration routes
// (tokens/charges/payouts/checkout-sessions) and merchant session tokens
// (internal/auth, Phase 2) for dashboard routes (api-key and
// webhook-endpoint management).
package publicapi

import (
	"context"
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
)

type ctxKey int

const (
	ctxKeyAPIKey ctxKey = iota
	ctxKeyMerchantID
)

func withAPIKey(ctx context.Context, key apikeys.ApiKey) context.Context {
	ctx = context.WithValue(ctx, ctxKeyAPIKey, key)
	return context.WithValue(ctx, ctxKeyMerchantID, key.MerchantID)
}

// apiKeyFromContext returns the authenticated API key for an integration
// route. Callers only reach this after apiKeyAuth middleware has run.
func apiKeyFromContext(r *http.Request) apikeys.ApiKey {
	key, _ := r.Context().Value(ctxKeyAPIKey).(apikeys.ApiKey)
	return key
}

func withMerchantID(ctx context.Context, merchantID string) context.Context {
	return context.WithValue(ctx, ctxKeyMerchantID, merchantID)
}

// merchantIDFromContext returns the merchant identity id for the current
// request, set by either apiKeyAuth or sessionAuth middleware.
func merchantIDFromContext(r *http.Request) string {
	id, _ := r.Context().Value(ctxKeyMerchantID).(string)
	return id
}
