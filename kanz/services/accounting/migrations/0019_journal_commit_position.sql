-- Commit progress is not financial knowledge time (#1309). Keep the WORM
-- journal unchanged and index new entries in a separate immutable append log.
-- The existing portfolio lock makes allocation order commit order; an ordinary
-- sequence would allow a smaller position to commit behind a checkpoint.
CREATE TABLE ledger_heads (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    position BIGINT NOT NULL CHECK (position >= 0),
    PRIMARY KEY (tenant_id, portfolio_id)
);
CREATE TABLE ledger_append_positions (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    position BIGINT NOT NULL CHECK (position > 0),
    entry_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, portfolio_id, position),
    UNIQUE (tenant_id, entry_id),
    FOREIGN KEY (tenant_id, entry_id) REFERENCES ledger_entries (tenant_id, entry_id)
);
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['ledger_heads', 'ledger_append_positions'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = app_current_tenant())
        WITH CHECK (tenant_id = app_current_tenant())
    $f$, t);
  END LOOP;
END $$;
CREATE TRIGGER ledger_append_positions_no_mutate
    BEFORE UPDATE OR DELETE ON ledger_append_positions
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER ledger_append_positions_no_truncate
    BEFORE TRUNCATE ON ledger_append_positions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm();

-- Initialize BEFORE the insert: an AFTER-row trigger for a multi-row INSERT
-- already sees the whole batch, so count(*)-1 there would create cursor gaps.
-- Duplicate attempts take the lock but only accepted rows advance in AFTER.
CREATE OR REPLACE FUNCTION ledger_prepare_append() RETURNS trigger AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext(NEW.portfolio_id));
    IF NOT EXISTS (SELECT 1 FROM ledger_heads WHERE tenant_id=NEW.tenant_id AND portfolio_id=NEW.portfolio_id) THEN
        INSERT INTO ledger_heads (tenant_id, portfolio_id, position)
        SELECT NEW.tenant_id, NEW.portfolio_id, count(*)
        FROM ledger_entries WHERE tenant_id=NEW.tenant_id AND portfolio_id=NEW.portfolio_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER ledger_entries_prepare_append BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_prepare_append();

CREATE OR REPLACE FUNCTION ledger_record_append() RETURNS trigger AS $$
DECLARE next_position bigint;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext(NEW.portfolio_id));
    UPDATE ledger_heads SET position=position+1
    WHERE tenant_id=NEW.tenant_id AND portfolio_id=NEW.portfolio_id
    RETURNING position INTO STRICT next_position;
    INSERT INTO ledger_append_positions (tenant_id, portfolio_id, position, entry_id)
    VALUES (NEW.tenant_id, NEW.portfolio_id, next_position, NEW.entry_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER ledger_entries_record_append AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_record_append();

ALTER TABLE ledger_snapshots ADD COLUMN journal_position BIGINT CHECK (journal_position >= 0);
-- Retire every tenant's derived fence without disabling FORCE RLS or touching
-- financial rows. Old readers also refuse these checkpoints immediately, even
-- before the first new append or background rebuild in that portfolio.
ALTER TABLE ledger_snapshots DROP COLUMN max_effective_time;
ALTER TABLE ledger_snapshots ADD COLUMN max_effective_time TIMESTAMPTZ;
-- Version 2 denotes a commit-position checkpoint. Old readers reject it and
-- replay the journal. Reject old writers too: an old UPSERT could otherwise
-- retain a new position while replacing the balances with an obsolete fold.
CREATE OR REPLACE FUNCTION ledger_fence_checkpoint() RETURNS trigger AS $$
DECLARE head_position bigint;
BEGIN
    IF TG_OP='UPDATE' AND NEW.corporate_action_version IS NULL THEN
        -- Allow the old/new action invalidation path, but leave no usable cursor.
        NEW.journal_position := NULL;
        RETURN NEW;
    END IF;
    IF NEW.corporate_action_version IS DISTINCT FROM 2 OR NEW.journal_position IS NULL THEN
        RAISE EXCEPTION 'ledger checkpoint requires commit-position version 2';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext(NEW.portfolio_id));
    SELECT position INTO head_position FROM ledger_heads
      WHERE tenant_id=NEW.tenant_id AND portfolio_id=NEW.portfolio_id;
    IF head_position IS NULL OR NEW.journal_position <> head_position THEN
        RAISE EXCEPTION 'stale ledger checkpoint position';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER ledger_snapshots_fence BEFORE INSERT OR UPDATE ON ledger_snapshots
    FOR EACH ROW EXECUTE FUNCTION ledger_fence_checkpoint();
