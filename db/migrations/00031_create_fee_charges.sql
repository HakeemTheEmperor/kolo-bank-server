-- +goose Up
-- Records a fee actually applied to a charge or payout. UNIQUE(source_type,
-- source_id) is the idempotency guard: ApplyFees checks NOT EXISTS against
-- this table rather than a new column on charges/payouts
-- (db/migrations/00024_create_charges.sql, 00025_create_payouts.sql),
-- keeping Phase 4/5 tables untouched.
CREATE TABLE fee_charges (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_type         TEXT NOT NULL CHECK (source_type IN ('charge', 'payout')),
    source_id           UUID NOT NULL,
    fee_rule_id         UUID REFERENCES fee_rules (id),
    fee_minor           BIGINT NOT NULL CHECK (fee_minor >= 0),
    tax_minor           BIGINT NOT NULL CHECK (tax_minor >= 0),
    total_minor         BIGINT NOT NULL CHECK (total_minor >= 0),
    currency            CHAR(3) NOT NULL,
    ledger_transaction_id UUID REFERENCES ledger_transactions (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_id)
);

-- +goose Down
DROP TABLE IF EXISTS fee_charges;
