-- +goose Up
-- Same shape as charges but outbound (see 00024_create_charges.sql).
CREATE TABLE payouts (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id         UUID NOT NULL REFERENCES identities (id),
    mode                TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    account_id          UUID NOT NULL REFERENCES accounts (id),
    rail_name           TEXT NOT NULL,
    recipient_ref       TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3) NOT NULL,
    idempotency_key     TEXT NOT NULL,
    external_transfer_id UUID NOT NULL REFERENCES external_transfers (id),
    notified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX idx_payouts_merchant_id ON payouts (merchant_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS payouts;
