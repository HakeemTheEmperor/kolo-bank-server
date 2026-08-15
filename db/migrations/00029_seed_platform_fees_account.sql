-- +goose Up
-- Platform revenue account for fees (docs/banking-backend-spec.md §4.1),
-- same pattern as the ledger's own system/suspense account
-- (db/migrations/00007_seed_system_accounts.sql).
INSERT INTO accounts (id, owner_id, type, currency, state, overdraft_limit_minor)
VALUES ('00000000-0000-0000-0000-000000000003',
        '00000000-0000-0000-0000-000000000000',
        'system', 'NGN', 'open', 0);

-- +goose Down
DELETE FROM accounts WHERE id = '00000000-0000-0000-0000-000000000003';
