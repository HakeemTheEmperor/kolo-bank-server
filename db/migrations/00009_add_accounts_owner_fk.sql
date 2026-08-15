-- +goose Up
ALTER TABLE accounts
    ADD CONSTRAINT fk_accounts_owner FOREIGN KEY (owner_id) REFERENCES identities (id);

-- +goose Down
ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS fk_accounts_owner;
