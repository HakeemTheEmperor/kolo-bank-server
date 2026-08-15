-- +goose Up
CREATE TABLE holds (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   UUID NOT NULL REFERENCES accounts (id),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'released', 'captured')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ
);

CREATE INDEX idx_holds_account_id_status ON holds (account_id, status);

-- +goose Down
DROP TABLE IF EXISTS holds;
