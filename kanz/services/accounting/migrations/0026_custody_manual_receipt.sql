-- Retain the canonical operator result even after a published outbox row is
-- removed. The (tenant_id, run_id) primary key arbitrates retries.
ALTER TABLE custody_runs ADD COLUMN manual_receipt BYTEA;
