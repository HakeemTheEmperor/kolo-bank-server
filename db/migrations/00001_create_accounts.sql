-- +goose Up
CREATE TABLE accounts (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id               UUID NOT NULL,
    type                   TEXT NOT NULL CHECK (type IN ('current', 'savings', 'wallet', 'sub_account', 'virtual', 'system')),
    currency               CHAR(3) NOT NULL,
    state                  TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'frozen', 'dormant', 'closed')),
    overdraft_limit_minor  BIGINT NOT NULL DEFAULT 0 CHECK (overdraft_limit_minor >= 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_accounts_owner_id ON accounts (owner_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_accounts_updated_at
    BEFORE UPDATE ON accounts
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_accounts_updated_at ON accounts;
DROP TABLE IF EXISTS accounts;
DROP FUNCTION IF EXISTS set_updated_at();
