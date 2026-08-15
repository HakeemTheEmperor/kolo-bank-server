-- +goose Up
CREATE TABLE identities (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           TEXT NOT NULL CHECK (kind IN ('individual', 'business', 'system')),
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'rejected', 'suspended', 'closed')),
    kyc_tier       INT NOT NULL DEFAULT 0 CHECK (kyc_tier IN (0, 1, 2)),
    email          TEXT NOT NULL UNIQUE,
    phone          TEXT,
    password_hash  TEXT NOT NULL,
    legal_name     TEXT NOT NULL,
    role           TEXT NOT NULL DEFAULT 'customer' CHECK (role IN ('customer', 'support_agent', 'admin')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_identities_updated_at
    BEFORE UPDATE ON identities
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- Matches the placeholder owner_id already used by the ledger's system
-- account (see db/migrations/00007_seed_system_accounts.sql), so the
-- upcoming accounts.owner_id -> identities.id foreign key is satisfied
-- without touching Phase 1 data.
INSERT INTO identities (id, kind, status, kyc_tier, email, password_hash, legal_name, role)
VALUES ('00000000-0000-0000-0000-000000000000',
        'system', 'active', 2, 'system@kolobank.internal', '', 'Kolo Bank System', 'admin');

-- +goose Down
DELETE FROM identities WHERE id = '00000000-0000-0000-0000-000000000000';
DROP TRIGGER IF EXISTS trg_identities_updated_at ON identities;
DROP TABLE IF EXISTS identities;
