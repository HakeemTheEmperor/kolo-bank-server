-- +goose Up
-- Every security-relevant action lands here, append-only
-- (docs/banking-backend-spec.md §3.2: "full audit logging of every auth
-- event"). actor_identity_id is nullable for system-initiated events.
CREATE TABLE audit_log (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_identity_id UUID REFERENCES identities (id),
    action            TEXT NOT NULL,
    target_type       TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_actor_identity_id ON audit_log (actor_identity_id);
CREATE INDEX idx_audit_log_target ON audit_log (target_type, target_id);

CREATE TRIGGER trg_audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW
    EXECUTE FUNCTION prevent_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS trg_audit_log_append_only ON audit_log;
DROP TABLE IF EXISTS audit_log;
