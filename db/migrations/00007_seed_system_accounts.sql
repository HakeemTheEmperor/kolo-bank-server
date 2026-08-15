-- +goose Up
-- A single-sided Credit/Debit (money entering/leaving the bank) must still
-- post as a balanced double-entry pair. Until Phase 4 wires real external
-- rails, the offsetting leg goes to this per-currency system/suspense
-- account, which is allowed to run arbitrarily negative (overdraft_limit_minor
-- set to the max int64) since it represents the outside world.
INSERT INTO accounts (id, owner_id, type, currency, state, overdraft_limit_minor)
VALUES ('00000000-0000-0000-0000-000000000001',
        '00000000-0000-0000-0000-000000000000',
        'system', 'NGN', 'open', 9223372036854775807);

-- +goose Down
DELETE FROM accounts WHERE id = '00000000-0000-0000-0000-000000000001';
