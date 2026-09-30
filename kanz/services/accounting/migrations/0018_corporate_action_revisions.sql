-- Old cached folds may include income as ex-date cash. Only a newly rebuilt
-- checkpoint may be resumed; the journal is immutable and is never rewritten.
ALTER TABLE ledger_snapshots ADD COLUMN IF NOT EXISTS corporate_action_version INTEGER;
ALTER TABLE ledger_snapshots ADD COLUMN IF NOT EXISTS action_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_snapshots ADD COLUMN IF NOT EXISTS next_effective_time TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS ledger_action_revision_idx ON ledger_entries
    (tenant_id, portfolio_id, (action->>'action_id'), ((action->>'revision')::numeric) DESC)
    WHERE action IS NOT NULL;
