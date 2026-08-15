-- +goose Up
-- Signed webhooks (docs/banking-backend-spec.md §3.6). secret is an HMAC
-- signing key shown once at creation, same "show once, store nothing raw"
-- pattern as api_keys.key_hash and sessions.token_hash — except here the
-- secret itself must be kept (not just hashed) to sign future deliveries,
-- so it's sealed via secrets.KeyProvider (internal/secrets), first reused
-- since internal/auth's TOTP secrets (00013_create_mfa.sql).
CREATE TABLE webhook_endpoints (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id      UUID NOT NULL REFERENCES identities (id),
    mode             TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    url              TEXT NOT NULL,
    secret_encrypted BYTEA NOT NULL,
    active           BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhook_endpoints_merchant_mode_active ON webhook_endpoints (merchant_id, mode) WHERE active;

-- +goose Down
DROP TABLE IF EXISTS webhook_endpoints;
