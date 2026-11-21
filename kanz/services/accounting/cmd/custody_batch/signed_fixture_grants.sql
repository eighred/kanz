-- Shared synthetic intake and break-action restrictions, included inside a provisioning transaction.
\set ON_ERROR_STOP on
\if :{?review_fence_only}
\else
\set review_fence_only off
\endif
DO $$
BEGIN
    IF current_database() <> 'kanz_custody_pilot' THEN
        RAISE EXCEPTION 'grant requires the isolated synthetic custody database';
    END IF;
END $$;

-- Older accounting replicas still accept operator actions. Every supported
-- claim or explanation inserts its receipt in the same transaction as the
-- break revision and FACT. Enforce the pilot's read-only boundary on all
-- runtime roles while allowing reconciliation and other tenants to continue.
DROP POLICY IF EXISTS signed_pilot_read_only_actions ON custody_actions;
CREATE POLICY signed_pilot_read_only_actions ON custody_actions AS RESTRICTIVE
    FOR INSERT TO PUBLIC WITH CHECK (tenant_id <> 'synthetic-custody-pilot');

\if :review_fence_only
\else
CREATE ROLE kanz_custody_batch_pilot NOLOGIN NOSUPERUSER NOBYPASSRLS;
SELECT pg_catalog.format(
    'GRANT USAGE ON SCHEMA %I TO kanz_custody_batch_pilot', pg_catalog.current_schema()
) \gexec
GRANT SELECT, INSERT ON custody_batch_receipts TO kanz_custody_batch_pilot;
CREATE POLICY signed_pilot_receipts ON custody_batch_receipts AS RESTRICTIVE
    TO kanz_custody_batch_pilot
    USING (tenant_id = 'synthetic-custody-pilot' AND portfolio_id = 'PF1' AND
           custodian_id = 'CUST-A' AND account_id = 'sandbox-account' AND
           source_id = 'synthetic-source' AND public_key = decode(:'public_key', 'base64'));

-- Outbox refuses unowned pending rows. Keep that invariant at storage even
-- though the source role can only see its own partition and envelope type.
ALTER TABLE outbox ADD CONSTRAINT signed_pilot_outbox_owner_required
    CHECK (tenant_id <> 'synthetic-custody-pilot' OR published_at IS NOT NULL OR producer_service IS NOT NULL);

GRANT USAGE ON SEQUENCE outbox_id_seq TO kanz_custody_batch_pilot;
GRANT SELECT, INSERT, UPDATE (attempts, published_at, last_error) ON outbox TO kanz_custody_batch_pilot;
CREATE POLICY signed_pilot_outbox ON outbox AS RESTRICTIVE TO kanz_custody_batch_pilot
    USING (tenant_id = 'synthetic-custody-pilot' AND envelope_tenant_id = tenant_id AND
           producer_service = 'custody-batch' AND partition_key = 'PF1' AND
           subject = 'accounting.custody.statement' AND event_type = subject AND
           domain = 'accounting' AND event_class = 1 AND schema_version = 1 AND
           payload_schema_ref = 'accounting.v1.CustodianStatement:1');
\endif
