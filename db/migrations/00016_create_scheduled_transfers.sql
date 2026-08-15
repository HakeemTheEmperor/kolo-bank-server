-- +goose Up
CREATE TABLE scheduled_transfers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_account_id UUID NOT NULL REFERENCES accounts (id),
    to_account_id   UUID NOT NULL REFERENCES accounts (id),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        CHAR(3) NOT NULL,
    schedule_type   TEXT NOT NULL CHECK (schedule_type IN ('once', 'recurring')),
    interval        TEXT CHECK (interval IN ('daily', 'weekly', 'monthly')),
    next_run_at     TIMESTAMPTZ NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'completed', 'cancelled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((schedule_type = 'once' AND interval IS NULL) OR (schedule_type = 'recurring' AND interval IS NOT NULL))
);

CREATE INDEX idx_scheduled_transfers_due ON scheduled_transfers (next_run_at) WHERE status = 'active';

CREATE TRIGGER trg_scheduled_transfers_updated_at
    BEFORE UPDATE ON scheduled_transfers
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_scheduled_transfers_updated_at ON scheduled_transfers;
DROP TABLE IF EXISTS scheduled_transfers;
