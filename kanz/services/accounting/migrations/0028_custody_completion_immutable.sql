-- A retained completion and the book scope that produced it are one audit
-- record. A correction appends a new run. Existing NULL receipts stay unknown.
CREATE TRIGGER custody_completion_immutable
    BEFORE UPDATE OR DELETE ON custody_runs
    FOR EACH ROW WHEN (OLD.manual_receipt IS NOT NULL)
    EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_completion_no_truncate
    BEFORE TRUNCATE ON custody_runs
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm();

-- Previous manual writers inserted the run, filled its receipt, and enqueued
-- the same bytes in one transaction. Check that path at commit, after enqueue.
CREATE OR REPLACE FUNCTION custody_check_legacy_completion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT starts_with(NEW.run_id, 'manual:') OR
       (to_jsonb(OLD) - 'manual_receipt') IS DISTINCT FROM (to_jsonb(NEW) - 'manual_receipt') OR
       NOT EXISTS (
           SELECT 1 FROM outbox WHERE tenant_id=NEW.tenant_id
             AND producer_service='accounting' AND event_type='accounting.custody.reconciled'
             AND correlation_id=NEW.run_id AND payload=NEW.manual_receipt
       ) THEN
        RAISE EXCEPTION 'custody completion must match its original manual FACT';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER custody_check_legacy_completion
    AFTER UPDATE ON custody_runs DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.manual_receipt IS NULL AND NEW.manual_receipt IS NOT NULL)
    EXECUTE FUNCTION custody_check_legacy_completion();
