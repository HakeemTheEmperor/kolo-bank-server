-- +goose Up
-- One TOTP factor per identity for now; secret_encrypted is sealed via
-- secrets.KeyProvider (docs/banking-backend-spec.md §3.12: HSM/KMS for
-- signing and encryption), never stored in plaintext.
CREATE TABLE mfa_totp_secrets (
    identity_id      UUID PRIMARY KEY REFERENCES identities (id),
    secret_encrypted BYTEA NOT NULL,
    confirmed        BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backs the stubbed SMS/push MFA channels: a challenge code is generated,
-- hashed at rest, and "delivered" by the stub notifier (never a real
-- external send, per the spec's non-goals around live rail integrations).
CREATE TABLE mfa_challenges (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id  UUID NOT NULL REFERENCES identities (id),
    channel      TEXT NOT NULL CHECK (channel IN ('sms', 'push')),
    code_hash    TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ
);

CREATE INDEX idx_mfa_challenges_identity_id ON mfa_challenges (identity_id);

-- +goose Down
DROP TABLE IF EXISTS mfa_challenges;
DROP TABLE IF EXISTS mfa_totp_secrets;
