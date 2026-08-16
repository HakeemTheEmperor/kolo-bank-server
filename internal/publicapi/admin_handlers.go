package publicapi

import (
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/resilience"
)

// resilienceStateResponse is the "what's currently affecting money
// movement" snapshot — GET /v1/admin/resilience.
type resilienceStateResponse struct {
	ReadOnly            bool                 `json:"read_only"`
	ReadOnlyReason      string               `json:"read_only_reason,omitempty"`
	ReadOnlyUpdatedAt   string               `json:"read_only_updated_at"`
	KillSwitchesTripped []killSwitchResponse `json:"kill_switches_tripped"`
}

type killSwitchResponse struct {
	ScopeType  string `json:"scope_type"`
	ScopeValue string `json:"scope_value"`
	Enabled    bool   `json:"enabled"`
	Reason     string `json:"reason,omitempty"`
	UpdatedBy  string `json:"updated_by"`
	UpdatedAt  string `json:"updated_at"`
}

func killSwitchToResponse(ks resilience.KillSwitch) killSwitchResponse {
	return killSwitchResponse{
		ScopeType:  string(ks.Scope.Type),
		ScopeValue: ks.Scope.Value,
		Enabled:    ks.Enabled,
		Reason:     ks.Reason,
		UpdatedBy:  ks.UpdatedBy,
		UpdatedAt:  ks.UpdatedAt.Format(rfc3339),
	}
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func (a *api) getResilienceState(w http.ResponseWriter, r *http.Request) {
	mode, err := a.deps.Resilience.GetSystemMode(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load system mode")
		return
	}
	all, err := a.deps.Resilience.ListKillSwitches(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load kill switches")
		return
	}

	tripped := make([]killSwitchResponse, 0)
	for _, ks := range all {
		if !ks.Enabled {
			tripped = append(tripped, killSwitchToResponse(ks))
		}
	}

	writeJSON(w, http.StatusOK, resilienceStateResponse{
		ReadOnly:            mode.ReadOnly,
		ReadOnlyReason:      mode.Reason,
		ReadOnlyUpdatedAt:   mode.UpdatedAt.Format(rfc3339),
		KillSwitchesTripped: tripped,
	})
}

func (a *api) listKillSwitches(w http.ResponseWriter, r *http.Request) {
	all, err := a.deps.Resilience.ListKillSwitches(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load kill switches")
		return
	}
	out := make([]killSwitchResponse, 0, len(all))
	for _, ks := range all {
		out = append(out, killSwitchToResponse(ks))
	}
	writeJSON(w, http.StatusOK, map[string]any{"kill_switches": out})
}

type setKillSwitchRequest struct {
	Enabled   bool   `json:"enabled"`
	Reason    string `json:"reason"`
	UpdatedBy string `json:"updated_by"`
}

// setKillSwitch is PUT /v1/admin/resilience/kill-switches/{scopeType}/{scopeValue} —
// creates the switch if it doesn't exist yet, or flips it if it does.
func (a *api) setKillSwitch(w http.ResponseWriter, r *http.Request) {
	scopeType := resilience.ScopeType(r.PathValue("scopeType"))
	scopeValue := r.PathValue("scopeValue")
	switch scopeType {
	case resilience.ScopeIntegration, resilience.ScopeMerchant, resilience.ScopeFeature:
	default:
		writeError(w, http.StatusBadRequest, "invalid_scope_type", `scope type must be "integration", "merchant", or "feature"`)
		return
	}

	var req setKillSwitchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	ks, err := a.deps.Resilience.SetKillSwitch(r.Context(), resilience.Scope{Type: scopeType, Value: scopeValue}, req.Enabled, req.Reason, req.UpdatedBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to set kill switch")
		return
	}
	writeJSON(w, http.StatusOK, killSwitchToResponse(ks))
}

type setReadOnlyRequest struct {
	Reason    string `json:"reason"`
	UpdatedBy string `json:"updated_by"`
}

type systemModeResponse struct {
	ReadOnly  bool   `json:"read_only"`
	Reason    string `json:"reason,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func systemModeToResponse(m resilience.SystemMode) systemModeResponse {
	return systemModeResponse{
		ReadOnly:  m.ReadOnly,
		Reason:    m.Reason,
		UpdatedBy: m.UpdatedBy,
		UpdatedAt: m.UpdatedAt.Format(rfc3339),
	}
}

func (a *api) enterReadOnly(w http.ResponseWriter, r *http.Request) {
	var req setReadOnlyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	mode, err := a.deps.Resilience.SetReadOnly(r.Context(), true, req.Reason, req.UpdatedBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to enter read-only mode")
		return
	}
	writeJSON(w, http.StatusOK, systemModeToResponse(mode))
}

func (a *api) exitReadOnly(w http.ResponseWriter, r *http.Request) {
	var req setReadOnlyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	mode, err := a.deps.Resilience.SetReadOnly(r.Context(), false, req.Reason, req.UpdatedBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to exit read-only mode")
		return
	}
	writeJSON(w, http.StatusOK, systemModeToResponse(mode))
}
