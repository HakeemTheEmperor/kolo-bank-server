-- +goose Up
-- Card authorization and settlement flows against a stubbed card-network
-- adapter (docs/banking-backend-spec.md §3.5). hold_id/ledger_transaction_id
-- reuse the existing holds/ledger_transactions primitives exactly as
-- internal/externalpayments already does for outbound rail transfers —
-- a card purchase is the same "money leaving the bank" shape.
CREATE TABLE card_authorizations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    card_id             UUID NOT NULL REFERENCES cards (id),
    account_id          UUID NOT NULL REFERENCES accounts (id),
    merchant_name       TEXT NOT NULL,
    mcc                 TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3) NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
        ('pending', 'requires_3ds', 'approved', 'declined', 'settled', 'voided', 'charged_back')),
    decline_reason      TEXT,
    hold_id             UUID REFERENCES holds (id),
    threeds_code_hash   TEXT,
    threeds_expires_at  TIMESTAMPTZ,
    ledger_transaction_id UUID REFERENCES ledger_transactions (id),
    idempotency_key     TEXT NOT NULL UNIQUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at          TIMESTAMPTZ
);

CREATE INDEX idx_card_authorizations_card_id ON card_authorizations (card_id, created_at);
CREATE INDEX idx_card_authorizations_account_daily ON card_authorizations (account_id, created_at) WHERE status = 'approved';

-- +goose Down
DROP TABLE IF EXISTS card_authorizations;
