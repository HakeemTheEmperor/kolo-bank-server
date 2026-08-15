-- +goose Up
-- API-key issuance, scoping, and rotation per merchant
-- (docs/banking-backend-spec.md §3.6). A "merchant" is simply a business
-- identity with keys attached — no separate merchant table. Only the hash
-- is ever stored; the raw key is shown once at creation/rotation, same
-- pattern as sessions.token_hash (00014_create_sessions.sql).
CREATE TABLE api_keys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID NOT NULL REFERENCES identities (id),
    mode        TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    name        TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,
    scopes      TEXT[] NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_merchant_id ON api_keys (merchant_id);

-- +goose Down
DROP TABLE IF EXISTS api_keys;
