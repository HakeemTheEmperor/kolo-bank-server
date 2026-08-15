-- +goose Up
CREATE TABLE ledger_transactions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key  TEXT NOT NULL UNIQUE,
    type             TEXT NOT NULL CHECK (type IN ('credit', 'debit', 'transfer', 'reversal')),
    state            TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'posted', 'failed', 'reversed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_ledger_transactions_updated_at
    BEFORE UPDATE ON ledger_transactions
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_ledger_transactions_updated_at ON ledger_transactions;
DROP TABLE IF EXISTS ledger_transactions;
