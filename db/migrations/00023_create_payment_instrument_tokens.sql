-- +goose Up
-- Tokenization of payment instruments (docs/banking-backend-spec.md §3.6).
-- Raw card data is never persisted: only a masked representation and a
-- deterministic will_fail flag derived once at creation from reserved
-- test card numbers (Stripe-style convention), the same
-- marker-in-input stub pattern used by kyc/compliance/rails.
CREATE TABLE payment_instrument_tokens (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id     UUID NOT NULL REFERENCES identities (id),
    mode            TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    masked_pan      TEXT NOT NULL,
    card_brand      TEXT NOT NULL,
    will_fail       BOOLEAN NOT NULL DEFAULT false,
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, idempotency_key)
);

-- +goose Down
DROP TABLE IF EXISTS payment_instrument_tokens;
