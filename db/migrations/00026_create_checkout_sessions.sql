-- +goose Up
-- Backend-only checkout session resource (docs/banking-backend-spec.md
-- §3.6). redirect_url is a placeholder a real hosted frontend would
-- render — no HTML is served by this backend (out of scope, see §1).
CREATE TABLE checkout_sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id     UUID NOT NULL REFERENCES identities (id),
    mode            TEXT NOT NULL CHECK (mode IN ('sandbox', 'live')),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        CHAR(3) NOT NULL,
    success_url     TEXT NOT NULL,
    cancel_url      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'completed', 'expired')),
    charge_id       UUID REFERENCES charges (id),
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (merchant_id, idempotency_key)
);

-- +goose Down
DROP TABLE IF EXISTS checkout_sessions;
