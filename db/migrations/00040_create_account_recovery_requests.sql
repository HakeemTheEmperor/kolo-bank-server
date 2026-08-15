-- +goose Up
-- Secure self-service account recovery via independent factors
-- (docs/banking-backend-spec.md §5.5): KYC re-verification + liveness
-- (kyc_result, from the same internal/kyc.Provider used at onboarding),
-- device history (device_fingerprint checked against internal/auth's
-- devices table), and a waiting period (eligible_at) before the identity's
-- credentials can actually be reset.
CREATE TABLE account_recovery_requests (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id       UUID NOT NULL REFERENCES identities (id),
    device_fingerprint TEXT NOT NULL,
    kyc_result        JSONB NOT NULL DEFAULT '[]',
    status            TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'rejected', 'waiting_period', 'eligible', 'completed')),
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    eligible_at       TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ
);

CREATE INDEX idx_account_recovery_requests_identity ON account_recovery_requests (identity_id);

-- +goose Down
DROP TABLE IF EXISTS account_recovery_requests;
