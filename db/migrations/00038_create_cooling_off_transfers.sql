-- +goose Up
-- Scam interruption and cooling-off on high-risk P2P transfers
-- (docs/banking-backend-spec.md §5.2). A high-risk send doesn't move money
-- immediately: internal/coolingoff places a hold (holds table,
-- 00004_create_holds.sql) and records the intent here instead, giving the
-- sender a short window to cancel before the release ticker captures the
-- hold into a real transfer.
CREATE TABLE cooling_off_transfers (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_account_id        UUID NOT NULL REFERENCES accounts (id),
    from_identity_id       UUID NOT NULL REFERENCES identities (id),
    to_account_id          UUID NOT NULL REFERENCES accounts (id),
    to_identity_id         UUID NOT NULL REFERENCES identities (id),
    amount_minor           BIGINT NOT NULL CHECK (amount_minor > 0),
    currency               CHAR(3) NOT NULL,
    reasons                JSONB NOT NULL DEFAULT '[]',
    hold_id                UUID NOT NULL REFERENCES holds (id),
    status                 TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'cancelled')),
    release_at             TIMESTAMPTZ NOT NULL,
    completed_transaction_id UUID REFERENCES ledger_transactions (id),
    idempotency_key        TEXT NOT NULL UNIQUE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cooling_off_transfers_due ON cooling_off_transfers (release_at) WHERE status = 'pending';
CREATE INDEX idx_cooling_off_transfers_from_identity ON cooling_off_transfers (from_identity_id, status);

-- +goose Down
DROP TABLE IF EXISTS cooling_off_transfers;
