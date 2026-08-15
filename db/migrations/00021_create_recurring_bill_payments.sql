-- +goose Up
CREATE TABLE recurring_bill_payments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    biller_id    UUID NOT NULL REFERENCES billers (id),
    account_id   UUID NOT NULL REFERENCES accounts (id),
    reference    TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency     CHAR(3) NOT NULL,
    interval     TEXT NOT NULL CHECK (interval IN ('daily', 'weekly', 'monthly')),
    next_run_at  TIMESTAMPTZ NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'cancelled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_recurring_bill_payments_due ON recurring_bill_payments (next_run_at) WHERE status = 'active';

CREATE TRIGGER trg_recurring_bill_payments_updated_at
    BEFORE UPDATE ON recurring_bill_payments
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_recurring_bill_payments_updated_at ON recurring_bill_payments;
DROP TABLE IF EXISTS recurring_bill_payments;
