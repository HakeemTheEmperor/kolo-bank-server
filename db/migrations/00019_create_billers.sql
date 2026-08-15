-- +goose Up
-- A stubbed biller directory (docs/banking-backend-spec.md §3.7). Real
-- biller integrations are a non-goal for this build (§1).
CREATE TABLE billers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    category   TEXT NOT NULL CHECK (category IN ('electricity', 'airtime', 'data', 'tv', 'water')),
    code       TEXT NOT NULL UNIQUE,
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO billers (name, category, code) VALUES
    ('National Electric', 'electricity', 'NATIONAL-ELECTRIC'),
    ('Kolo Mobile Airtime', 'airtime', 'KOLO-AIRTIME'),
    ('Kolo Mobile Data', 'data', 'KOLO-DATA'),
    ('National TV', 'tv', 'NATIONAL-TV');

-- +goose Down
DROP TABLE IF EXISTS billers;
