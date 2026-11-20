-- Lookup hints only. Replay compares retained FACT bytes before suppressing a fill.
-- Existing FACTs stay immutable and continue to carry their original terms.
ALTER TABLE tv_facts ADD COLUMN fill_alias_digest bytea,
                     ADD COLUMN execution_key_digest bytea,
                     ADD CHECK (fill_alias_digest IS NULL OR octet_length(fill_alias_digest)=32),
                     ADD CHECK (execution_key_digest IS NULL OR
                         (fill_alias_digest IS NOT NULL AND octet_length(execution_key_digest)=32));
CREATE INDEX tv_facts_fill_alias_idx ON tv_facts(tenant_id, fill_alias_digest, seq)
    WHERE fill_alias_digest IS NOT NULL;
CREATE INDEX tv_facts_execution_key_idx ON tv_facts(tenant_id, execution_key_digest, seq)
    WHERE execution_key_digest IS NOT NULL;
CREATE INDEX tv_facts_unindexed_fill_idx ON tv_facts(tenant_id, seq)
    WHERE fill_alias_digest IS NULL AND event_type IN ('order.order.filled', 'order.order.partially_filled');

-- Replay depends on both indexed and historical FACTs. Runtime grants on other
-- TV tables must not permit deleting an old claim or detaching its lookup key.
CREATE FUNCTION tv_preserve_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'TV execution fact log is immutable';
END;
$$;
CREATE TRIGGER tv_preserve_fact BEFORE UPDATE OR DELETE ON tv_facts
FOR EACH ROW EXECUTE FUNCTION tv_preserve_fact();
