-- +goose Up
CREATE TABLE idempotency_keys (
    key            TEXT PRIMARY KEY,
    request_hash   TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'in_progress' CHECK (status IN ('in_progress', 'completed')),
    response_body  BYTEA,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys (expires_at);

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
