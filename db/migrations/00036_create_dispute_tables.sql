-- +goose Up
-- Dispute and case management for back-office investigators
-- (docs/banking-backend-spec.md §3.8). source_type/source_id references
-- whatever the dispute is about (a charge, payout, or external transfer)
-- without a table-specific FK, the same source_type/source_id shape as
-- fee_charges (00031_create_fee_charges.sql). dispute_events is an
-- append-only timeline of state transitions, mirroring the audit trail's
-- append-only design (internal/audit).
CREATE TABLE disputes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id  UUID NOT NULL REFERENCES identities (id),
    source_type  TEXT NOT NULL CHECK (source_type IN ('charge', 'payout', 'external_transfer')),
    source_id    UUID NOT NULL,
    reason       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'investigating', 'resolved', 'rejected')),
    resolution   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_disputes_updated_at
    BEFORE UPDATE ON disputes
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE INDEX idx_disputes_identity_id ON disputes (identity_id);
CREATE INDEX idx_disputes_status ON disputes (status);

CREATE TABLE dispute_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dispute_id   UUID NOT NULL REFERENCES disputes (id),
    from_status  TEXT NOT NULL,
    to_status    TEXT NOT NULL,
    note         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dispute_events_dispute_id ON dispute_events (dispute_id);

-- +goose Down
DROP TABLE IF EXISTS dispute_events;
DROP TRIGGER IF EXISTS trg_disputes_updated_at ON disputes;
DROP TABLE IF EXISTS disputes;
