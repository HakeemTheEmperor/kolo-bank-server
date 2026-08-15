package publicapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/toluwalase/kolo-bank-server/internal/cards"
)

type issueCardRequest struct {
	AccountID string `json:"account_id"`
	CardType  string `json:"card_type"`
}

type cardResponse struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	CardType  string `json:"card_type"`
	PANLast4  string `json:"pan_last4"`
	Brand     string `json:"brand"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func cardToResponse(c cards.Card) cardResponse {
	return cardResponse{
		ID: c.ID, AccountID: c.AccountID, CardType: string(c.CardType), PANLast4: c.PANLast4,
		Brand: c.Brand, Status: string(c.Status), CreatedAt: c.CreatedAt.Format(timeFormat),
	}
}

func (a *api) issueCard(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	var req issueCardRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	cardType := cards.CardType(req.CardType)
	if cardType != cards.CardTypeVirtual && cardType != cards.CardTypePhysical {
		writeError(w, http.StatusBadRequest, "invalid_card_type", `card_type must be "virtual" or "physical"`)
		return
	}

	owns, err := a.ownsAccount(r, identityID, req.AccountID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "verify account ownership failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to verify account")
		return
	}
	if !owns {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}

	c, err := a.deps.Cards.Issue(r.Context(), identityID, req.AccountID, cardType)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "issue card failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to issue card")
		return
	}
	writeJSON(w, http.StatusCreated, cardToResponse(c))
}

func (a *api) listCards(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	list, err := a.deps.Cards.ListByIdentity(r.Context(), identityID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list cards failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list cards")
		return
	}
	out := make([]cardResponse, len(list))
	for i, c := range list {
		out[i] = cardToResponse(c)
	}
	writeJSON(w, http.StatusOK, out)
}

// ownsCard verifies cardID belongs to identityID, writing a 404 (never a
// 403 — same "don't confirm existence" reasoning as ownsAPIKey) if not.
func (a *api) ownsCard(w http.ResponseWriter, r *http.Request, identityID, cardID string) (cards.Card, bool) {
	c, err := a.deps.Cards.Get(r.Context(), cardID)
	if err != nil {
		if errors.Is(err, cards.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "card not found")
			return cards.Card{}, false
		}
		a.deps.Logger.ErrorContext(r.Context(), "load card failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load card")
		return cards.Card{}, false
	}
	if c.IdentityID != identityID {
		writeError(w, http.StatusNotFound, "not_found", "card not found")
		return cards.Card{}, false
	}
	return c, true
}

func (a *api) freezeCard(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	if _, ok := a.ownsCard(w, r, identityID, r.PathValue("id")); !ok {
		return
	}
	if err := a.deps.Cards.Freeze(r.Context(), r.PathValue("id")); err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "freeze card failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to freeze card")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) unfreezeCard(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	if _, ok := a.ownsCard(w, r, identityID, r.PathValue("id")); !ok {
		return
	}
	if err := a.deps.Cards.Unfreeze(r.Context(), r.PathValue("id")); err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "unfreeze card failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to unfreeze card")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type setCardLimitsRequest struct {
	DailyLimitMinor          int64    `json:"daily_limit_minor"`
	PerTransactionLimitMinor int64    `json:"per_transaction_limit_minor"`
	BlockedMCCs              []string `json:"blocked_mccs"`
}

func (a *api) setCardLimits(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	if _, ok := a.ownsCard(w, r, identityID, r.PathValue("id")); !ok {
		return
	}
	var req setCardLimitsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if req.DailyLimitMinor <= 0 || req.PerTransactionLimitMinor <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_limits", "limits must be positive")
		return
	}

	if err := a.deps.Cards.SetLimits(r.Context(), r.PathValue("id"), req.DailyLimitMinor, req.PerTransactionLimitMinor, req.BlockedMCCs); err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "set card limits failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to set limits")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type cardAuthorizationResponse struct {
	ID            string `json:"id"`
	MerchantName  string `json:"merchant_name"`
	MCC           string `json:"mcc"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	DeclineReason string `json:"decline_reason,omitempty"`
	CreatedAt     string `json:"created_at"`
}

func authorizationToResponse(a cards.Authorization) cardAuthorizationResponse {
	resp := cardAuthorizationResponse{
		ID: a.ID, MerchantName: a.MerchantName, MCC: a.MCC, AmountMinor: a.Amount.Minor, Currency: a.Amount.Currency,
		Status: string(a.Status), CreatedAt: a.CreatedAt.Format(timeFormat),
	}
	if a.DeclineReason != "" {
		resp.DeclineReason = a.DeclineReason
	}
	return resp
}

func (a *api) listCardAuthorizations(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	card, ok := a.ownsCard(w, r, identityID, r.PathValue("id"))
	if !ok {
		return
	}

	list, err := a.deps.Cards.ListAuthorizationsByCard(r.Context(), card.ID)
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list card authorizations failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list authorizations")
		return
	}
	out := make([]cardAuthorizationResponse, len(list))
	for i, auth := range list {
		out[i] = authorizationToResponse(auth)
	}
	writeJSON(w, http.StatusOK, out)
}

type complete3DSRequest struct {
	Code string `json:"code"`
}

func (a *api) completeCard3DS(w http.ResponseWriter, r *http.Request) {
	identityID := merchantIDFromContext(r)
	authID := r.PathValue("id")

	auth, err := a.deps.Cards.GetAuthorization(r.Context(), authID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "authorization not found")
		return
	}
	if _, ok := a.ownsCard(w, r, identityID, auth.CardID); !ok {
		return
	}

	var req complete3DSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	result, err := a.deps.Cards.Complete3DS(r.Context(), authID, req.Code)
	if err != nil {
		if errors.Is(err, cards.ErrNotAuthorized) {
			writeError(w, http.StatusConflict, "not_eligible", "authorization is not awaiting 3ds")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "complete 3ds failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to complete 3ds")
		return
	}
	writeJSON(w, http.StatusOK, authorizationToResponse(result))
}

// ownsAccount verifies accountID belongs to identityID before a card can
// be issued against it.
func (a *api) ownsAccount(r *http.Request, identityID, accountID string) (bool, error) {
	var owns bool
	if err := a.deps.Pool.QueryRow(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1 AND owner_id = $2)
	`, accountID, identityID).Scan(&owns); err != nil {
		return false, err
	}
	return owns, nil
}
