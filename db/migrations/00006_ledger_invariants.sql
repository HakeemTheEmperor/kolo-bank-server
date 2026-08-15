-- +goose Up
-- Invariant 1: every ledger transaction's entries must sum to zero (balanced
-- double-entry). Deferred so all rows in a multi-entry INSERT (e.g. a
-- transfer's debit + credit) are checked once, at commit.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_check_balanced() RETURNS TRIGGER AS $$
DECLARE
    total BIGINT;
BEGIN
    SELECT COALESCE(SUM(amount_minor), 0) INTO total
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger_entries: transaction % is not balanced (sum = %)', NEW.transaction_id, total
            USING ERRCODE = 'B0003';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_check_balanced();

-- Invariant 2: an account's available balance (posted ledger balance minus
-- active holds) must never drop below its authorized overdraft limit.
-- Locks the account row (SELECT ... FOR UPDATE) so concurrent debits against
-- the same account serialize instead of both reading a stale balance.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_check_available_balance() RETURNS TRIGGER AS $$
DECLARE
    ledger_balance    BIGINT;
    resulting_balance BIGINT;
    held              BIGINT;
    overdraft         BIGINT;
BEGIN
    IF NEW.amount_minor > 0 THEN
        RETURN NEW;
    END IF;

    SELECT overdraft_limit_minor INTO overdraft
      FROM accounts
     WHERE id = NEW.account_id
       FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: account % does not exist', NEW.account_id
            USING ERRCODE = 'B0002';
    END IF;

    SELECT COALESCE(SUM(amount_minor), 0) INTO ledger_balance
      FROM ledger_entries
     WHERE account_id = NEW.account_id;

    SELECT COALESCE(SUM(amount_minor), 0) INTO held
      FROM holds
     WHERE account_id = NEW.account_id
       AND status = 'active';

    resulting_balance := ledger_balance + NEW.amount_minor;

    IF (resulting_balance - held) < (-overdraft) THEN
        RAISE EXCEPTION
            'ledger_entries: account % available balance would fall below overdraft limit (ledger=%, held=%, overdraft=%)',
            NEW.account_id, resulting_balance, held, overdraft
            USING ERRCODE = 'B0001';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_ledger_entries_available_balance
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION ledger_check_available_balance();

-- +goose Down
DROP TRIGGER IF EXISTS trg_ledger_entries_available_balance ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_check_available_balance();
DROP TRIGGER IF EXISTS trg_ledger_entries_balanced ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_check_balanced();
