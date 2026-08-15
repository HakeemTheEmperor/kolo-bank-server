-- +goose Up
CREATE TABLE webhook_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID NOT NULL REFERENCES identities (id),
    mode        TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    event_type  TEXT NOT NULL CHECK (event_type IN ('charge.succeeded', 'charge.failed', 'payout.succeeded', 'payout.failed')),
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per (event, endpoint) delivery attempt series, with
-- exponential backoff on failure. A row stuck 'pending' with a due
-- next_attempt_at is exactly what DeliverPending claims and retries;
-- attempt_count hitting max_attempts is what finally gives up.
CREATE TABLE webhook_deliveries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_event_id    UUID NOT NULL REFERENCES webhook_events (id),
    webhook_endpoint_id UUID NOT NULL REFERENCES webhook_endpoints (id),
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempt_count       INT NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ
);

CREATE INDEX idx_webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
