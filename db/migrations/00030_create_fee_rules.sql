-- +goose Up
-- Rule-based fee resolution (docs/banking-backend-spec.md §4.1): tiered
-- pricing, per-merchant overrides, caps, and taxes. merchant_id NULL is
-- the default/tiered rule; rail '*' matches any rail. Resolve picks the
-- most specific match: merchant-specific over default, exact rail over
-- wildcard.
CREATE TABLE fee_rules (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id      UUID REFERENCES identities (id),
    flow             TEXT NOT NULL CHECK (flow IN ('charge', 'payout')),
    rail             TEXT NOT NULL DEFAULT '*',
    currency         CHAR(3) NOT NULL,
    min_amount_minor BIGINT NOT NULL DEFAULT 0,
    max_amount_minor BIGINT,
    percent_bps      INT NOT NULL DEFAULT 0 CHECK (percent_bps >= 0),
    fixed_minor      BIGINT NOT NULL DEFAULT 0 CHECK (fixed_minor >= 0),
    cap_minor        BIGINT CHECK (cap_minor IS NULL OR cap_minor >= 0),
    tax_percent_bps  INT NOT NULL DEFAULT 0 CHECK (tax_percent_bps >= 0),
    active           BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_fee_rules_lookup ON fee_rules (flow, currency, rail) WHERE active;

-- Default rules applied when no merchant-specific override exists:
-- 1.5% + ₦100 fixed on charges (capped at ₦2,000), flat ₦50 on payouts.
INSERT INTO fee_rules (flow, rail, currency, percent_bps, fixed_minor, cap_minor, tax_percent_bps)
VALUES ('charge', '*', 'NGN', 150, 100_00, 2000_00, 750);

INSERT INTO fee_rules (flow, rail, currency, percent_bps, fixed_minor, tax_percent_bps)
VALUES ('payout', '*', 'NGN', 0, 50_00, 750);

-- +goose Down
DROP TABLE IF EXISTS fee_rules;
