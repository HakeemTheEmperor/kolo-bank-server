-- +goose Up
-- Widens disputes' source_type CHECK constraint (00036_create_dispute_tables.sql)
-- to match what internal/publicapi's createDispute handler already accepts.
-- Phase 8 added "cooling_off_transfer" as an allowed customer-dispute
-- source at the HTTP layer but never widened this constraint, so that path
-- has been failing at the DB layer since Phase 8 (untested — no existing
-- test exercised it). This fixes that gap and adds "card_authorization"
-- for Phase 9's chargebacks in the same pass.
ALTER TABLE disputes DROP CONSTRAINT disputes_source_type_check;
ALTER TABLE disputes ADD CONSTRAINT disputes_source_type_check
    CHECK (source_type IN ('charge', 'payout', 'external_transfer', 'cooling_off_transfer', 'card_authorization'));

-- +goose Down
ALTER TABLE disputes DROP CONSTRAINT disputes_source_type_check;
ALTER TABLE disputes ADD CONSTRAINT disputes_source_type_check
    CHECK (source_type IN ('charge', 'payout', 'external_transfer'));
