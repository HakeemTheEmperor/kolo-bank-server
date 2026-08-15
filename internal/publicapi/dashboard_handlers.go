package publicapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

// requireMerchant verifies the session-authenticated identity is a
// business identity — only merchants integrate, individuals don't get API
// keys or webhook endpoints.
func (a *api) requireMerchant(w http.ResponseWriter, r *http.Request) (merchantID string, ok bool) {
	merchantID = merchantIDFromContext(r)
	id, err := a.deps.Identity.GetByID(r.Context(), merchantID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "load identity failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load identity")
		return "", false
	}
	if id.Kind != identity.KindBusiness {
		writeError(w, http.StatusForbidden, "forbidden", "only business identities can manage API keys and webhook endpoints")
		return "", false
	}
	return merchantID, true
}

// --- API keys ---

type createAPIKeyRequest struct {
	Mode   string   `json:"mode"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type apiKeyResponse struct {
	ID        string   `json:"id"`
	Mode      string   `json:"mode"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

// createAPIKeyResponse additionally carries the raw key, shown exactly
// once.
type createAPIKeyResponse struct {
	apiKeyResponse
	Key string `json:"key"`
}

func apiKeyToResponse(k apikeys.ApiKey) apiKeyResponse {
	return apiKeyResponse{
		ID: k.ID, Mode: string(k.Mode), Name: k.Name, KeyPrefix: k.KeyPrefix,
		Scopes: k.Scopes, Status: string(k.Status), CreatedAt: k.CreatedAt.Format(timeFormat),
	}
}

func (a *api) createAPIKey(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	var req createAPIKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	mode := apikeys.Mode(req.Mode)
	if mode != apikeys.ModeSandbox && mode != apikeys.ModeLive {
		writeError(w, http.StatusBadRequest, "invalid_mode", `mode must be "sandbox" or "live"`)
		return
	}

	raw, key, err := a.deps.ApiKeys.Create(r.Context(), merchantID, mode, req.Name, req.Scopes)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "create api key failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create API key")
		return
	}

	writeJSON(w, http.StatusCreated, createAPIKeyResponse{apiKeyResponse: apiKeyToResponse(key), Key: raw})
}

func (a *api) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	keys, err := a.deps.ApiKeys.List(r.Context(), merchantID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list api keys failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list API keys")
		return
	}
	out := make([]apiKeyResponse, len(keys))
	for i, k := range keys {
		out[i] = apiKeyToResponse(k)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) rotateAPIKey(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	if !a.ownsAPIKey(w, r, merchantID, r.PathValue("id")) {
		return
	}

	raw, key, err := a.deps.ApiKeys.Rotate(r.Context(), r.PathValue("id"))
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "rotate api key failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to rotate API key")
		return
	}
	writeJSON(w, http.StatusOK, createAPIKeyResponse{apiKeyResponse: apiKeyToResponse(key), Key: raw})
}

func (a *api) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	if !a.ownsAPIKey(w, r, merchantID, r.PathValue("id")) {
		return
	}

	if err := a.deps.ApiKeys.Revoke(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, apikeys.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "API key not found")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "revoke api key failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to revoke API key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownsAPIKey enforces that keyID belongs to merchantID before a
// rotate/revoke proceeds — without this, a valid session could act on any
// other merchant's key by guessing its id.
func (a *api) ownsAPIKey(w http.ResponseWriter, r *http.Request, merchantID, keyID string) bool {
	key, err := a.deps.ApiKeys.Get(r.Context(), keyID)
	if err != nil {
		if errors.Is(err, apikeys.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "API key not found")
			return false
		}
		a.deps.Logger.ErrorContext(r.Context(), "load api key failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load API key")
		return false
	}
	if key.MerchantID != merchantID {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return false
	}
	return true
}

// --- webhook endpoints ---

type createWebhookEndpointRequest struct {
	Mode string `json:"mode"`
	URL  string `json:"url"`
}

type webhookEndpointResponse struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	URL       string `json:"url"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

type createWebhookEndpointResponse struct {
	webhookEndpointResponse
	Secret string `json:"secret"`
}

func webhookEndpointToResponse(e webhooks.Endpoint) webhookEndpointResponse {
	return webhookEndpointResponse{ID: e.ID, Mode: string(e.Mode), URL: e.URL, Active: e.Active, CreatedAt: e.CreatedAt.Format(timeFormat)}
}

func (a *api) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	var req createWebhookEndpointRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	mode := apikeys.Mode(req.Mode)
	if mode != apikeys.ModeSandbox && mode != apikeys.ModeLive {
		writeError(w, http.StatusBadRequest, "invalid_mode", `mode must be "sandbox" or "live"`)
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid_url", "url is required")
		return
	}

	secret, ep, err := a.deps.Webhooks.CreateEndpoint(r.Context(), merchantID, mode, req.URL)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "create webhook endpoint failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create webhook endpoint")
		return
	}

	writeJSON(w, http.StatusCreated, createWebhookEndpointResponse{webhookEndpointResponse: webhookEndpointToResponse(ep), Secret: secret})
}

func (a *api) listWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	endpoints, err := a.deps.Webhooks.ListEndpoints(r.Context(), merchantID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list webhook endpoints failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list webhook endpoints")
		return
	}
	out := make([]webhookEndpointResponse, len(endpoints))
	for i, e := range endpoints {
		out[i] = webhookEndpointToResponse(e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) deactivateWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := a.requireMerchant(w, r)
	if !ok {
		return
	}

	endpoints, err := a.deps.Webhooks.ListEndpoints(r.Context(), merchantID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list webhook endpoints failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load webhook endpoint")
		return
	}
	owned := false
	for _, e := range endpoints {
		if e.ID == r.PathValue("id") {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
		return
	}

	if err := a.deps.Webhooks.DeactivateEndpoint(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "deactivate webhook endpoint failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to deactivate webhook endpoint")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
