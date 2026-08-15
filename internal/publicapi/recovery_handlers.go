package publicapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/recovery"
)

// Account recovery is deliberately public/unauthenticated
// (docs/banking-backend-spec.md §5.5) — it exists precisely for the case
// where the customer can't log in — and rate-limited by IP via
// ipRateLimit rather than the session/API-key limiter, since there's
// nothing authenticated yet to key on.

type initiateRecoveryRequest struct {
	Email             string `json:"email"`
	DeviceFingerprint string `json:"device_fingerprint"`
	LegalName         string `json:"legal_name"`
	Address           string `json:"address"`
}

type recoveryRequestResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	EligibleAt string `json:"eligible_at"`
}

func (a *api) initiateRecovery(w http.ResponseWriter, r *http.Request) {
	var req initiateRecoveryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	result, err := a.deps.Recovery.Initiate(r.Context(), req.Email, req.DeviceFingerprint, req.LegalName, req.Address)
	if err != nil && !errors.Is(err, recovery.ErrKYCFailed) {
		if errors.Is(err, identity.ErrNotFound) {
			// Deliberately the same response shape as a real request, to
			// avoid confirming or denying whether an email is registered.
			writeJSON(w, http.StatusAccepted, recoveryRequestResponse{Status: "pending"})
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "initiate recovery failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to initiate recovery")
		return
	}

	writeJSON(w, http.StatusAccepted, recoveryRequestResponse{
		ID: result.ID, Status: string(result.Status), EligibleAt: result.EligibleAt.Format(timeFormat),
	})
}

type completeRecoveryRequest struct {
	NewPassword string `json:"new_password"`
}

func (a *api) completeRecovery(w http.ResponseWriter, r *http.Request) {
	var req completeRecoveryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "invalid_password", "new_password must be at least 8 characters")
		return
	}

	if err := a.deps.Recovery.Complete(r.Context(), r.PathValue("id"), req.NewPassword); err != nil {
		if errors.Is(err, recovery.ErrNotEligible) {
			writeError(w, http.StatusConflict, "not_eligible", "recovery request is not yet eligible to complete")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "complete recovery failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to complete recovery")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
