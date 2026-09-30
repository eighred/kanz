-- Old cached folds may include income as ex-date cash. Only a newly rebuilt
-- checkpoint may be resumed; the journal is immutable and is never rewritten.
ALTER TABLE ledger_snapshots ADD COLUMN IF NOT EXISTS corporate_action_version INTEGER;
CREATE INDEX IF NOT EXISTS ledger_action_revision_idx ON ledger_entries
    (tenant_id, portfolio_id, (action->>'action_id'), ((action->>'revision')::numeric) DESC)
    WHERE action IS NOT NULL;
