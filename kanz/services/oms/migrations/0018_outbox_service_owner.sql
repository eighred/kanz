-- #1325: ownership travels with each FACT in the domain transaction.
-- Legacy rows deliberately remain NULL: neither subjects nor the migration's
-- service name proves which producer originally committed an ambiguous row.
ALTER TABLE outbox ADD COLUMN producer_service TEXT;

-- NOT VALID preserves unclassified history but checks every new/updated row.
-- Old binaries cannot silently add more unowned FACTs after this migration.
ALTER TABLE outbox ADD CONSTRAINT outbox_producer_service_required
    CHECK (producer_service IS NOT NULL AND producer_service <> ''
           AND producer_service = btrim(producer_service)) NOT VALID;

-- A known owner is immutable. Reconciliation may classify NULL legacy rows,
-- but it may not transfer a committed FACT between producers.
CREATE OR REPLACE FUNCTION outbox_preserve_producer_service() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.producer_service IS NOT NULL
       AND NEW.producer_service IS DISTINCT FROM OLD.producer_service THEN
        RAISE EXCEPTION 'outbox producer service is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_producer_service_immutable
    BEFORE UPDATE OF producer_service ON outbox
    FOR EACH ROW EXECUTE FUNCTION outbox_preserve_producer_service();

CREATE INDEX outbox_service_pending_idx
    ON outbox (tenant_id, producer_service, partition_key, id)
    WHERE published_at IS NULL;
CREATE INDEX outbox_unowned_pending_idx ON outbox (tenant_id, id)
    WHERE published_at IS NULL AND producer_service IS NULL;

-- Stop all writers/relays before upgrading. The new enqueue and relay refuse
-- while this tenant has unowned pending rows. Reconcile those against original
-- transaction evidence before assigning an owner; never default the backlog.
-- Published history may remain unclassified. See internal/outbox/README.md.
