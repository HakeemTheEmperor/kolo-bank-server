-- +goose Up
-- token_hash is the only thing ever persisted; the raw bearer token is
-- returned to the caller once at Login and never stored or logged.
CREATE TABLE sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id   UUID NOT NULL REFERENCES identities (id),
    device_id     UUID REFERENCES devices (id),
    token_hash    TEXT NOT NULL UNIQUE,
    mfa_verified  BOOLEAN NOT NULL DEFAULT false,
    step_up_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX idx_sessions_identity_id ON sessions (identity_id);

-- +goose Down
DROP TABLE IF EXISTS sessions;
