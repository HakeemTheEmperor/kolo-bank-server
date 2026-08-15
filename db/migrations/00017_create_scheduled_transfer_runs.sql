-- +goose Up
-- One row per firing of a scheduled_transfer. The UNIQUE constraint below is
-- the concurrency guard: two overlapping RunDue calls both trying to claim
-- the same (schedule, occurrence) race on this insert, and only one wins,
-- so a schedule can never double-fire even under concurrent execution.
-- A run stuck in 'processing' past a timeout is what the in-flight
-- resolver (ResolveStuck) sweeps (docs/banking-backend-spec.md §4.4).
CREATE TABLE scheduled_transfer_runs (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheduled_transfer_id UUID NOT NULL REFERENCES scheduled_transfers (id),
    scheduled_for        TIMESTAMPTZ NOT NULL,
    status               TEXT NOT NULL DEFAULT 'processing' CHECK (status IN ('processing', 'completed', 'failed')),
    transaction_id       UUID REFERENCES ledger_transactions (id),
    attempted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at         TIMESTAMPTZ,
    error                TEXT,
    UNIQUE (scheduled_transfer_id, scheduled_for)
);

CREATE INDEX idx_scheduled_transfer_runs_stuck ON scheduled_transfer_runs (attempted_at) WHERE status = 'processing';

-- +goose Down
DROP TABLE IF EXISTS scheduled_transfer_runs;
