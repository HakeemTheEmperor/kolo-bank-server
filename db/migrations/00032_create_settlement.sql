-- +goose Up
-- Settlement engine with rolling reserves (docs/banking-backend-spec.md
-- §4.2). A merchant's collections aren't paid out instantly: they
-- aggregate per cycle, net of fees, with a percentage held back as a
-- reserve against future chargebacks and released after reserve_hold_days.
CREATE TABLE merchant_settlement_configs (
    merchant_id        UUID PRIMARY KEY REFERENCES identities (id),
    currency            CHAR(3) NOT NULL,
    reserve_percent_bps INT NOT NULL DEFAULT 0 CHECK (reserve_percent_bps >= 0 AND reserve_percent_bps <= 10000),
    reserve_hold_days   INT NOT NULL DEFAULT 7 CHECK (reserve_hold_days >= 0),
    recipient_ref       TEXT NOT NULL,
    rail                TEXT NOT NULL,
    cycle_interval       TEXT NOT NULL CHECK (cycle_interval IN ('daily', 'weekly')),
    next_cycle_at        TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_merchant_settlement_configs_updated_at
    BEFORE UPDATE ON merchant_settlement_configs
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TABLE settlement_cycles (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id        UUID NOT NULL REFERENCES identities (id),
    currency           CHAR(3) NOT NULL,
    gross_minor        BIGINT NOT NULL CHECK (gross_minor >= 0),
    fees_minor         BIGINT NOT NULL CHECK (fees_minor >= 0),
    reserve_minor      BIGINT NOT NULL CHECK (reserve_minor >= 0),
    net_minor          BIGINT NOT NULL CHECK (net_minor >= 0),
    payout_id          UUID REFERENCES payouts (id),
    reserve_release_at TIMESTAMPTZ NOT NULL,
    reserve_released   BOOLEAN NOT NULL DEFAULT false,
    reserve_payout_id  UUID REFERENCES payouts (id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_settlement_cycles_merchant_id ON settlement_cycles (merchant_id);
CREATE INDEX idx_settlement_cycles_reserve_due ON settlement_cycles (reserve_release_at) WHERE NOT reserve_released;

-- Which charges were rolled into which cycle — a join table rather than a
-- column on charges (db/migrations/00024_create_charges.sql), the same
-- non-invasive reasoning as fee_charges.
CREATE TABLE settlement_cycle_items (
    settlement_cycle_id UUID NOT NULL REFERENCES settlement_cycles (id),
    charge_id           UUID PRIMARY KEY REFERENCES charges (id)
);

CREATE INDEX idx_settlement_cycle_items_cycle_id ON settlement_cycle_items (settlement_cycle_id);

-- +goose Down
DROP TABLE IF EXISTS settlement_cycle_items;
DROP TABLE IF EXISTS settlement_cycles;
DROP TRIGGER IF EXISTS trg_merchant_settlement_configs_updated_at ON merchant_settlement_configs;
DROP TABLE IF EXISTS merchant_settlement_configs;
