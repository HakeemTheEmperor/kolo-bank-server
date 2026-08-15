-- +goose Up
-- Automated multi-way reconciliation with break detection
-- (docs/banking-backend-spec.md §4.3). statement_lines simulate the
-- partner-bank/rail statement — a source independent of our own ledger —
-- generated from external_transfers (db/migrations/00018_create_external_transfers.sql).
CREATE TABLE reconciliation_statement_lines (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_transfer_id UUID NOT NULL REFERENCES external_transfers (id),
    reported_amount_minor BIGINT NOT NULL,
    currency            CHAR(3) NOT NULL,
    status              TEXT NOT NULL DEFAULT 'unmatched' CHECK (status IN ('unmatched', 'matched', 'break')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (external_transfer_id)
);

CREATE INDEX idx_reconciliation_statement_lines_unmatched ON reconciliation_statement_lines (created_at) WHERE status = 'unmatched';

-- The escalation queue: genuine mismatches that auto-resolution could not
-- explain away as benign timing.
CREATE TABLE reconciliation_breaks (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_line_id     UUID NOT NULL REFERENCES reconciliation_statement_lines (id),
    external_transfer_id  UUID NOT NULL REFERENCES external_transfers (id),
    reason                TEXT NOT NULL,
    expected_amount_minor BIGINT NOT NULL,
    reported_amount_minor BIGINT NOT NULL,
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at           TIMESTAMPTZ
);

CREATE INDEX idx_reconciliation_breaks_open ON reconciliation_breaks (created_at) WHERE status = 'open';

-- +goose Down
DROP TABLE IF EXISTS reconciliation_breaks;
DROP TABLE IF EXISTS reconciliation_statement_lines;
