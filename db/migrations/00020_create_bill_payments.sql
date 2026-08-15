-- +goose Up
-- Records a bill/airtime/data payment submission. The definitive outcome
-- (completed/failed) lives on the linked external_transfers row — this
-- table intentionally doesn't duplicate that status, to avoid two copies
-- of the same fact drifting out of sync; it doubles as recurring
-- payments' own run history via its unique idempotency_key (see
-- 00021_create_recurring_bill_payments.sql).
CREATE TABLE bill_payments (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    biller_id           UUID NOT NULL REFERENCES billers (id),
    account_id          UUID NOT NULL REFERENCES accounts (id),
    reference           TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3) NOT NULL,
    idempotency_key     TEXT NOT NULL UNIQUE,
    external_transfer_id UUID REFERENCES external_transfers (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_bill_payments_account_id ON bill_payments (account_id);

-- +goose Down
DROP TABLE IF EXISTS bill_payments;
