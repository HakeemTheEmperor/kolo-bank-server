package publicapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/coolingoff"
	"github.com/toluwalase/kolo-bank-server/internal/disputes"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
)

// resolveOwnAccount finds the session identity's earliest open account —
// the same "no primary account concept yet" simplification
// internal/payments.SendToRecipientEmail already documents.
func (a *api) resolveOwnAccount(w http.ResponseWriter, r *http.Request, identityID string) (string, bool) {
	var accountID string
	err := a.deps.Pool.QueryRow(r.Context(), `
		SELECT id::text FROM accounts WHERE owner_id = $1 AND state = 'open' ORDER BY created_at LIMIT 1
	`, identityID).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnprocessableEntity, "no_account", "you have no open account")
			return "", false
		}
		a.deps.Logger.ErrorContext(r.Context(), "resolve own account failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to resolve account")
		return "", false
	}
	return accountID, true
}

// --- cooling-off transfers (§5.1 confirmation of payee, §5.2 cooling-off) ---

type createTransferRequest struct {
	RecipientEmail  string `json:"recipient_email"`
	RecipientName   string `json:"recipient_name"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	ConfirmMismatch bool   `json:"confirm_mismatch"`
}

type transferResponse struct {
	Outcome       string   `json:"outcome"`
	PayeeResult   string   `json:"payee_result"`
	Reasons       []string `json:"reasons,omitempty"`
	TransactionID string   `json:"transaction_id,omitempty"`
	PendingID     string   `json:"pending_id,omitempty"`
	ReleaseAt     string   `json:"release_at,omitempty"`
}

func (a *api) createTransfer(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)

	var req createTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	amount, err := ledger.NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", err.Error())
		return
	}

	fromAccountID, ok := a.resolveOwnAccount(w, r, identityID)
	if !ok {
		return
	}

	result, err := a.deps.CoolingOff.Send(r.Context(), fromAccountID, identityID, req.RecipientEmail, req.RecipientName, amount, r.Header.Get("Idempotency-Key"), req.ConfirmMismatch)
	if err != nil {
		switch {
		case errors.Is(err, coolingoff.ErrRecipientNotFound):
			writeError(w, http.StatusNotFound, "not_found", "recipient not found")
		case errors.Is(err, coolingoff.ErrPayeeMismatchNotConfirmed):
			writeJSON(w, http.StatusConflict, transferResponse{Outcome: "blocked", PayeeResult: string(result.PayeeResult)})
		case errors.Is(err, payments.ErrLimitExceeded):
			writeError(w, http.StatusUnprocessableEntity, "limit_exceeded", err.Error())
		case errors.Is(err, ledger.ErrInsufficientBalance):
			writeError(w, http.StatusUnprocessableEntity, "insufficient_balance", "insufficient available balance")
		default:
			a.deps.Logger.ErrorContext(r.Context(), "create transfer failed", slog.Any("error", err))
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to send transfer")
		}
		return
	}

	resp := transferResponse{Outcome: string(result.Outcome), PayeeResult: string(result.PayeeResult), Reasons: result.Reasons}
	if result.Outcome == coolingoff.OutcomeCompleted {
		resp.TransactionID = result.Transaction.ID
	} else {
		resp.PendingID = result.PendingID
		resp.ReleaseAt = result.ReleaseAt.Format(timeFormat)
	}
	writeJSON(w, http.StatusCreated, resp)
}

type pendingTransferResponse struct {
	ID          string   `json:"id"`
	ToAccountID string   `json:"to_account_id"`
	AmountMinor int64    `json:"amount_minor"`
	Currency    string   `json:"currency"`
	Reasons     []string `json:"reasons"`
	Status      string   `json:"status"`
	ReleaseAt   string   `json:"release_at"`
	CreatedAt   string   `json:"created_at"`
}

func (a *api) listPendingTransfers(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	pending, err := a.deps.CoolingOff.ListPendingByIdentity(r.Context(), identityID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list pending transfers failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list pending transfers")
		return
	}
	out := make([]pendingTransferResponse, len(pending))
	for i, p := range pending {
		out[i] = pendingTransferResponse{
			ID: p.ID, ToAccountID: p.ToAccountID, AmountMinor: p.AmountMinor, Currency: p.Currency,
			Reasons: p.Reasons, Status: p.Status, ReleaseAt: p.ReleaseAt.Format(timeFormat), CreatedAt: p.CreatedAt.Format(timeFormat),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	if err := a.deps.CoolingOff.Cancel(r.Context(), r.PathValue("id"), identityID); err != nil {
		if errors.Is(err, coolingoff.ErrNotPending) {
			writeError(w, http.StatusNotFound, "not_found", "no cancellable transfer with that id")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "cancel transfer failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to cancel transfer")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- connected apps & consent (§5.3) ---

type grantAuthorizationRequest struct {
	Scopes []string `json:"scopes"`
}

type authorizationResponse struct {
	ID         string   `json:"id"`
	MerchantID string   `json:"merchant_id"`
	Scopes     []string `json:"scopes"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at,omitempty"`
}

func (a *api) grantAuthorization(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	var req grantAuthorizationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if len(req.Scopes) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_scopes", "at least one scope is required")
		return
	}

	grant, err := a.deps.Consent.Grant(r.Context(), identityID, r.PathValue("merchantID"), req.Scopes)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "grant authorization failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to grant authorization")
		return
	}
	resp := authorizationResponse{ID: grant.ID, MerchantID: grant.MerchantID, Scopes: grant.Scopes, Status: string(grant.Status), CreatedAt: grant.CreatedAt.Format(timeFormat)}
	if grant.LastUsedAt != nil {
		s := grant.LastUsedAt.Format(timeFormat)
		resp.LastUsedAt = &s
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (a *api) listAuthorizations(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	authorizations, err := a.deps.Consent.ListByIdentity(r.Context(), identityID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list authorizations failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list authorizations")
		return
	}
	out := make([]authorizationResponse, len(authorizations))
	for i, auth := range authorizations {
		out[i] = authorizationResponse{
			ID: auth.ID, MerchantID: auth.MerchantID, Scopes: auth.Scopes, Status: string(auth.Status),
			CreatedAt: auth.CreatedAt.Format(timeFormat),
		}
		if auth.LastUsedAt != nil {
			s := auth.LastUsedAt.Format(timeFormat)
			out[i].LastUsedAt = &s
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) revokeAuthorization(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	if err := a.deps.Consent.Revoke(r.Context(), identityID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "authorization not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- self-service disputes (§5.4) ---

type createDisputeRequest struct {
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id"`
	Reason     string `json:"reason"`
}

type disputeResponse struct {
	ID         string  `json:"id"`
	SourceType string  `json:"source_type"`
	SourceID   string  `json:"source_id"`
	Reason     string  `json:"reason"`
	Status     string  `json:"status"`
	Resolution *string `json:"resolution,omitempty"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

func disputeToResponse(d disputes.Dispute) disputeResponse {
	return disputeResponse{
		ID: d.ID, SourceType: d.SourceType, SourceID: d.SourceID, Reason: d.Reason,
		Status: string(d.Status), Resolution: d.Resolution,
		CreatedAt: d.CreatedAt.Format(timeFormat), UpdatedAt: d.UpdatedAt.Format(timeFormat),
	}
}

func (a *api) createDispute(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	var req createDisputeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if req.SourceType != "external_transfer" && req.SourceType != "cooling_off_transfer" && req.SourceType != "card_authorization" {
		writeError(w, http.StatusBadRequest, "invalid_source_type", `source_type must be "external_transfer", "cooling_off_transfer", or "card_authorization"`)
		return
	}

	owns, err := a.ownsTransfer(r, identityID, req.SourceType, req.SourceID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "verify transfer ownership failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to verify ownership")
		return
	}
	if !owns {
		writeError(w, http.StatusNotFound, "not_found", "transfer not found")
		return
	}

	d, err := a.deps.Disputes.Create(r.Context(), identityID, req.SourceType, req.SourceID, req.Reason)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "create dispute failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create dispute")
		return
	}
	writeJSON(w, http.StatusCreated, disputeToResponse(d))
}

// ownsTransfer confirms sourceID's underlying account belongs to
// identityID before a dispute can be raised against it — without this, a
// customer could dispute someone else's transaction by guessing its id.
func (a *api) ownsTransfer(r *http.Request, identityID, sourceType, sourceID string) (bool, error) {
	var query string
	switch sourceType {
	case "external_transfer":
		query = `SELECT EXISTS(SELECT 1 FROM external_transfers et JOIN accounts a ON a.id = et.account_id WHERE et.id = $1 AND a.owner_id = $2)`
	case "cooling_off_transfer":
		query = `SELECT EXISTS(SELECT 1 FROM cooling_off_transfers WHERE id = $1 AND from_identity_id = $2)`
	case "card_authorization":
		query = `SELECT EXISTS(SELECT 1 FROM card_authorizations ca JOIN accounts a ON a.id = ca.account_id WHERE ca.id = $1 AND a.owner_id = $2)`
	default:
		return false, nil
	}
	var owns bool
	if err := a.deps.Pool.QueryRow(r.Context(), query, sourceID, identityID).Scan(&owns); err != nil {
		return false, err
	}
	return owns, nil
}

const defaultDisputeListLimit = 50

func (a *api) listDisputes(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	list, err := a.deps.Disputes.ListByIdentity(r.Context(), identityID, defaultDisputeListLimit)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list disputes failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list disputes")
		return
	}
	out := make([]disputeResponse, len(list))
	for i, d := range list {
		out[i] = disputeToResponse(d)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) getDispute(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	d, err := a.deps.Disputes.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "dispute not found")
		return
	}
	if d.IdentityID != identityID {
		writeError(w, http.StatusNotFound, "not_found", "dispute not found")
		return
	}
	writeJSON(w, http.StatusOK, disputeToResponse(d))
}
