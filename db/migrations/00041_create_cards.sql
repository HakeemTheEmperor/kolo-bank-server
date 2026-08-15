-- +goose Up
-- Virtual and physical card issuing, with per-card controls
-- (docs/banking-backend-spec.md §3.5). card_limits is a one-row-per-card
-- child table (same shape as merchant_settlement_configs,
-- 00032_create_settlement.sql) rather than columns on cards, so limits can
-- be updated independently of the card record.
CREATE TABLE cards (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id  UUID NOT NULL REFERENCES identities (id),
    account_id   UUID NOT NULL REFERENCES accounts (id),
    card_type    TEXT NOT NULL CHECK (card_type IN ('virtual', 'physical')),
    pan_last4    CHAR(4) NOT NULL,
    brand        TEXT NOT NULL DEFAULT 'kolocard',
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_cards_updated_at
    BEFORE UPDATE ON cards
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE INDEX idx_cards_identity_id ON cards (identity_id);

CREATE TABLE card_limits (
    card_id                   UUID PRIMARY KEY REFERENCES cards (id),
    daily_limit_minor         BIGINT NOT NULL CHECK (daily_limit_minor > 0),
    per_transaction_limit_minor BIGINT NOT NULL CHECK (per_transaction_limit_minor > 0),
    blocked_mccs              TEXT[] NOT NULL DEFAULT '{}'
);

-- +goose Down
DROP TABLE IF EXISTS card_limits;
DROP TRIGGER IF EXISTS trg_cards_updated_at ON cards;
DROP TABLE IF EXISTS cards;
