-- +goose Up
CREATE TABLE beneficial_owners (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    business_identity_id UUID NOT NULL REFERENCES identities (id),
    full_name            TEXT NOT NULL,
    ownership_percent    NUMERIC(5, 2) NOT NULL CHECK (ownership_percent > 0 AND ownership_percent <= 100),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_beneficial_owners_business_identity_id ON beneficial_owners (business_identity_id);

-- +goose Down
DROP TABLE IF EXISTS beneficial_owners;
