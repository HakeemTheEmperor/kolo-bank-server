package publicapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
)

// --- tokens ---

type createTokenRequest struct {
	CardNumber string `json:"card_number"`
}

type tokenResponse struct {
	ID        string `json:"id"`
	MaskedPAN string `json:"masked_pan"`
	CardBrand string `json:"card_brand"`
	CreatedAt string `json:"created_at"`
}

func (a *api) createToken(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	var req createTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	tok, err := a.deps.Tokens.Create(r.Context(), key.MerchantID, key.Mode, req.CardNumber, r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, tokens.ErrInvalidCardNumber) {
			writeError(w, http.StatusBadRequest, "invalid_card_number", "card number must be 12-19 digits")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "create token failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create token")
		return
	}

	writeJSON(w, http.StatusCreated, tokenResponse{
		ID: tok.ID, MaskedPAN: tok.MaskedPAN, CardBrand: tok.CardBrand, CreatedAt: tok.CreatedAt.Format(timeFormat),
	})
}

// --- charges ---

type createChargeRequest struct {
	TokenID     string `json:"token_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type chargeResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	TokenID     string `json:"token_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	CreatedAt   string `json:"created_at"`
}

func chargeToResponse(c charges.Charge) chargeResponse {
	return chargeResponse{
		ID: c.ID, Status: string(c.Status), TokenID: c.TokenID,
		AmountMinor: c.Amount.Minor, Currency: c.Amount.Currency, CreatedAt: c.CreatedAt.Format(timeFormat),
	}
}

func (a *api) createCharge(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	var req createChargeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	amount, err := ledger.NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", err.Error())
		return
	}

	ch, err := a.deps.Charges.Create(r.Context(), key.MerchantID, key.Mode, req.TokenID, amount, r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, charges.ErrNoSettlementAccount) {
			writeError(w, http.StatusUnprocessableEntity, "no_settlement_account", "merchant has no open settlement account for this currency")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "create charge failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create charge")
		return
	}

	writeJSON(w, http.StatusCreated, chargeToResponse(ch))
}

func (a *api) getCharge(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	ch, err := a.deps.Charges.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "charge not found")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "get charge failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch charge")
		return
	}
	if ch.MerchantID != key.MerchantID {
		writeError(w, http.StatusNotFound, "not_found", "charge not found")
		return
	}
	writeJSON(w, http.StatusOK, chargeToResponse(ch))
}

func (a *api) listCharges(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	list, err := a.deps.Charges.List(r.Context(), key.MerchantID, key.Mode, listLimit(r))
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list charges failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list charges")
		return
	}
	out := make([]chargeResponse, len(list))
	for i, c := range list {
		out[i] = chargeToResponse(c)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- payouts ---

type createPayoutRequest struct {
	Rail         string `json:"rail"`
	RecipientRef string `json:"recipient_ref"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
}

type payoutResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Rail         string `json:"rail"`
	RecipientRef string `json:"recipient_ref"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	CreatedAt    string `json:"created_at"`
}

func payoutToResponse(p payouts.Payout) payoutResponse {
	return payoutResponse{
		ID: p.ID, Status: string(p.Status), Rail: p.RailName, RecipientRef: p.RecipientRef,
		AmountMinor: p.Amount.Minor, Currency: p.Amount.Currency, CreatedAt: p.CreatedAt.Format(timeFormat),
	}
}

func (a *api) createPayout(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	var req createPayoutRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	amount, err := ledger.NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", err.Error())
		return
	}

	p, err := a.deps.Payouts.Create(r.Context(), key.MerchantID, key.Mode, req.Rail, req.RecipientRef, amount, r.Header.Get("Idempotency-Key"))
	if err != nil {
		switch {
		case errors.Is(err, payouts.ErrUnknownRail):
			writeError(w, http.StatusBadRequest, "unknown_rail", "unrecognized rail")
		case errors.Is(err, payouts.ErrNoSettlementAccount):
			writeError(w, http.StatusUnprocessableEntity, "no_settlement_account", "merchant has no open settlement account for this currency")
		default:
			a.deps.Logger.ErrorContext(r.Context(), "create payout failed", slog.Any("error", err))
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to create payout")
		}
		return
	}

	writeJSON(w, http.StatusCreated, payoutToResponse(p))
}

func (a *api) getPayout(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	p, err := a.deps.Payouts.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "payout not found")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "get payout failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch payout")
		return
	}
	if p.MerchantID != key.MerchantID {
		writeError(w, http.StatusNotFound, "not_found", "payout not found")
		return
	}
	writeJSON(w, http.StatusOK, payoutToResponse(p))
}

func (a *api) listPayouts(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	list, err := a.deps.Payouts.List(r.Context(), key.MerchantID, key.Mode, listLimit(r))
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "list payouts failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list payouts")
		return
	}
	out := make([]payoutResponse, len(list))
	for i, p := range list {
		out[i] = payoutToResponse(p)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- checkout sessions ---

type createCheckoutSessionRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

type checkoutSessionResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	RedirectURL string `json:"redirect_url"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
}

func (a *api) checkoutToResponse(s checkout.Session) checkoutSessionResponse {
	return checkoutSessionResponse{
		ID: s.ID, Status: string(s.Status), AmountMinor: s.Amount.Minor, Currency: s.Amount.Currency,
		RedirectURL: s.RedirectURL(a.deps.PublicBaseURL), CreatedAt: s.CreatedAt.Format(timeFormat), ExpiresAt: s.ExpiresAt.Format(timeFormat),
	}
}

func (a *api) createCheckoutSession(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	var req createCheckoutSessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	amount, err := ledger.NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", err.Error())
		return
	}

	sess, err := a.deps.Checkout.Create(r.Context(), key.MerchantID, key.Mode, amount, req.SuccessURL, req.CancelURL, r.Header.Get("Idempotency-Key"))
	if err != nil {
		a.deps.Logger.ErrorContext(r.Context(), "create checkout session failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create checkout session")
		return
	}

	writeJSON(w, http.StatusCreated, a.checkoutToResponse(sess))
}

func (a *api) getCheckoutSession(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromContext(r)
	sess, err := a.deps.Checkout.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, checkout.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "checkout session not found")
			return
		}
		a.deps.Logger.ErrorContext(r.Context(), "get checkout session failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch checkout session")
		return
	}
	if sess.MerchantID != key.MerchantID {
		writeError(w, http.StatusNotFound, "not_found", "checkout session not found")
		return
	}
	writeJSON(w, http.StatusOK, a.checkoutToResponse(sess))
}
