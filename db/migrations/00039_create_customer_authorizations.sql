-- +goose Up
-- Connected-apps & consent dashboard (docs/banking-backend-spec.md §5.3):
-- every third-party (merchant) authorization a customer has granted via
-- the API, viewable and one-tap revocable. Standalone OAuth-style grant,
-- deliberately not layered onto payment_instrument_tokens or api_keys —
-- neither has any concept of a specific customer identity today.
CREATE TABLE customer_authorizations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id   UUID NOT NULL REFERENCES identities (id),
    merchant_id   UUID NOT NULL REFERENCES identities (id),
    scopes        TEXT[] NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    UNIQUE (identity_id, merchant_id)
);

CREATE INDEX idx_customer_authorizations_identity ON customer_authorizations (identity_id, status);

-- +goose Down
DROP TABLE IF EXISTS customer_authorizations;
