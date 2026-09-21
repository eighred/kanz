ALTER TABLE ledger_entries ADD COLUMN execution_evidence BYTEA;
CREATE INDEX ledger_execution_alias_idx ON ledger_entries(tenant_id,source_ref)
    WHERE entry_type=1 AND source_ref<>'';

-- Older binaries use fill:<raw alias>, whereas new writers scope an execution
-- by venue/account/instrument. Unknown legacy attribution fails closed during
-- rollout rather than letting the two identities create two cash postings.
CREATE OR REPLACE FUNCTION protect_ledger_execution_rollout() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE alias TEXT;
BEGIN
    IF NEW.entry_type<>1 OR NEW.entry_id NOT LIKE 'fill:%' THEN RETURN NEW; END IF;
    alias := COALESCE(NULLIF(NEW.source_ref,''),substring(NEW.entry_id FROM 6));
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id),hashtext('ledger-execution-alias:' || alias));
    IF NEW.entry_id='fill:' || alias THEN
        IF EXISTS (SELECT 1 FROM ledger_entries WHERE tenant_id=NEW.tenant_id
                   AND source_ref=alias AND entry_type=1 AND entry_id<>NEW.entry_id) THEN
            RAISE EXCEPTION 'legacy ledger writer cannot attribute a scoped execution alias';
        END IF;
    ELSIF EXISTS (SELECT 1 FROM ledger_entries WHERE tenant_id=NEW.tenant_id AND entry_id='fill:' || alias) THEN
        RAISE EXCEPTION 'legacy execution requires proven attribution before scoped ledger posting';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER ledger_execution_rollout BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION protect_ledger_execution_rollout();
