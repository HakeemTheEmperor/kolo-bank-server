-- +goose Up
-- Tracks money crossing the bank's boundary via a simulated external rail
-- (docs/banking-backend-spec.md §3.4, §4.4). Mirrors the
-- claim -> external-call -> finalize shape of scheduled_transfer_runs
-- (00017_create_scheduled_transfer_runs.sql): a row stuck in 'processing'
-- past a timeout is exactly the crash/timeout window the in-flight
-- resolver exists to clean up.
CREATE TABLE external_transfers (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    direction           TEXT NOT NULL CHECK (direction IN ('outbound', 'inbound')),
    account_id          UUID NOT NULL REFERENCES accounts (id),
    rail_name           TEXT NOT NULL,
    counterparty_ref    TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3) NOT NULL,
    idempotency_key     TEXT NOT NULL UNIQUE,
    hold_id             UUID REFERENCES holds (id),
    batch_id            UUID,
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    rail_reference      TEXT,
    ledger_transaction_id UUID REFERENCES ledger_transactions (id),
    error               TEXT,
    attempted_at        TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_external_transfers_pending ON external_transfers (created_at) WHERE status = 'pending';
CREATE INDEX idx_external_transfers_stuck ON external_transfers (attempted_at) WHERE status = 'processing';
CREATE INDEX idx_external_transfers_batch_id ON external_transfers (batch_id);

-- +goose Down
DROP TABLE IF EXISTS external_transfers;
