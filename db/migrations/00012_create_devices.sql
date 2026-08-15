-- +goose Up
CREATE TABLE devices (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id    UUID NOT NULL REFERENCES identities (id),
    fingerprint    TEXT NOT NULL,
    trusted        BOOLEAN NOT NULL DEFAULT false,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (identity_id, fingerprint)
);

-- +goose Down
DROP TABLE IF EXISTS devices;
