-- +goose Up
-- AML/regulatory reporting exports (docs/banking-backend-spec.md §3.8):
-- SAR (suspicious activity, from blocked risk_assessments) and CTR
-- (currency transaction reporting, from large external_transfers) equivalents.
-- UNIQUE(report_type, period_start, period_end) is the idempotency guard
-- against double-generating the same period's report, the same pattern as
-- fee_charges'/risk_assessments' UNIQUE(source_type, source_id).
CREATE TABLE regulatory_reports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    report_type  TEXT NOT NULL CHECK (report_type IN ('sar', 'ctr')),
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload      JSONB NOT NULL,
    UNIQUE (report_type, period_start, period_end)
);

-- +goose Down
DROP TABLE IF EXISTS regulatory_reports;
