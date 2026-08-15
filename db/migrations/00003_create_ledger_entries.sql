-- +goose Up
-- Append-only journal of balanced debit/credit entries. Balances are always
-- derived by summing this table; rows are never updated or deleted
-- (see docs/banking-backend-spec.md §6).
CREATE TABLE ledger_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id  UUID NOT NULL REFERENCES ledger_transactions (id),
    account_id      UUID NOT NULL REFERENCES accounts (id),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor <> 0),
    currency        CHAR(3) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_transaction_id ON ledger_entries (transaction_id);
CREATE INDEX idx_ledger_entries_account_id ON ledger_entries (account_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; % is not allowed', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION prevent_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS trg_ledger_entries_append_only ON ledger_entries;
DROP FUNCTION IF EXISTS prevent_mutation();
DROP TABLE IF EXISTS ledger_entries;
