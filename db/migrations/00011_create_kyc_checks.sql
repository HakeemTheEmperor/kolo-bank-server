-- +goose Up
-- Append-only record of every KYC/compliance provider call
-- (docs/banking-backend-spec.md §3.8: "an audit trail for everything").
CREATE TABLE kyc_checks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id UUID NOT NULL REFERENCES identities (id),
    check_type  TEXT NOT NULL CHECK (check_type IN ('id_document', 'liveness', 'address', 'sanctions', 'pep')),
    provider    TEXT NOT NULL,
    result      TEXT NOT NULL CHECK (result IN ('pass', 'fail', 'review')),
    raw_result  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_kyc_checks_identity_id ON kyc_checks (identity_id);

CREATE TRIGGER trg_kyc_checks_append_only
    BEFORE UPDATE OR DELETE ON kyc_checks
    FOR EACH ROW
    EXECUTE FUNCTION prevent_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS trg_kyc_checks_append_only ON kyc_checks;
DROP TABLE IF EXISTS kyc_checks;
