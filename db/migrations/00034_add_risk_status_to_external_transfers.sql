-- +goose Up
-- Real-time hold/block gate for money leaving the bank's boundary
-- (docs/banking-backend-spec.md §3.8, Phase 7). Additive column with a
-- default that preserves every existing row and query: internal/risk
-- assesses a claimed external_transfers row before it's finalized, and
-- ProcessPending's claim query (internal/externalpayments/process.go)
-- additionally requires risk_status = 'clear'.
ALTER TABLE external_transfers
    ADD COLUMN risk_status TEXT NOT NULL DEFAULT 'clear' CHECK (risk_status IN ('clear', 'held', 'blocked'));

CREATE INDEX idx_external_transfers_held ON external_transfers (created_at) WHERE risk_status = 'held';

-- +goose Down
DROP INDEX IF EXISTS idx_external_transfers_held;
ALTER TABLE external_transfers DROP COLUMN IF EXISTS risk_status;
