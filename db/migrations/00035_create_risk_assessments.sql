-- +goose Up
-- Real-time transaction monitoring and fraud scoring
-- (docs/banking-backend-spec.md §3.8). One row per assessed
-- external_transfers row, recording the rules that fired and the decision
-- reached. UNIQUE(source_type, source_id) is the idempotency guard,
-- mirroring fee_charges (00031_create_fee_charges.sql): a retried Assess
-- call for the same transfer returns the existing row instead of
-- re-scoring or double-releasing a hold.
CREATE TABLE risk_assessments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_type  TEXT NOT NULL CHECK (source_type IN ('external_transfer')),
    source_id    UUID NOT NULL,
    score        INT NOT NULL CHECK (score >= 0 AND score <= 100),
    decision     TEXT NOT NULL CHECK (decision IN ('allow', 'hold', 'block')),
    reasons      JSONB NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_id)
);

CREATE INDEX idx_risk_assessments_decision ON risk_assessments (decision, created_at);

-- +goose Down
DROP TABLE IF EXISTS risk_assessments;
