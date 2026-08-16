-- +goose Up
-- Phase 10 kill switches and system-wide read-only mode
-- (docs/banking-backend-spec.md §Phase 10): per-integration, per-merchant,
-- and per-feature circuit breakers flippable without a deploy, plus a
-- singleton read-only-mode row. Checked synchronously by
-- internal/resilience.Service.Check at the top of money-moving service
-- methods (see that package's doc comment for exactly which call sites).
CREATE TABLE kill_switches (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_type   TEXT NOT NULL CHECK (scope_type IN ('integration', 'merchant', 'feature')),
    scope_value  TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    reason       TEXT,
    updated_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope_type, scope_value)
);

CREATE TRIGGER trg_kill_switches_updated_at
    BEFORE UPDATE ON kill_switches
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE INDEX idx_kill_switches_scope ON kill_switches (scope_type, scope_value) WHERE enabled = false;

-- Singleton row (fixed boolean PK, always true) rather than a flag bolted
-- onto some other table, so entering/exiting read-only mode is auditable
-- the same way kill switches are (who, when, why).
CREATE TABLE system_mode (
    id           BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    read_only    BOOLEAN NOT NULL DEFAULT false,
    reason       TEXT,
    updated_by   TEXT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_system_mode_updated_at
    BEFORE UPDATE ON system_mode
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

INSERT INTO system_mode (id, read_only) VALUES (true, false);

-- +goose Down
DROP TABLE IF EXISTS system_mode;
DROP TABLE IF EXISTS kill_switches;
