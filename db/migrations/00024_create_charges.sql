-- +goose Up
-- A charge is a thin record over an inbound external_transfers row (see
-- 00018_create_external_transfers.sql); its status is read by joining to
-- that row, never duplicated here — same decision bill_payments already
-- made (00020_create_bill_payments.sql). notified_at tracks whether a
-- webhook has been enqueued for this charge's terminal outcome.
CREATE TABLE charges (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id         UUID NOT NULL REFERENCES identities (id),
    mode                TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    token_id            UUID NOT NULL REFERENCES payment_instrument_tokens (id),
    account_id          UUID NOT NULL REFERENCES accounts (id),
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3) NOT NULL,
    idempotency_key     TEXT NOT NULL,
    external_transfer_id UUID NOT NULL REFERENCES external_transfers (id),
    notified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX idx_charges_merchant_id ON charges (merchant_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS charges;
