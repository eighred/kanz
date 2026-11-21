-- Explicit administrator provisioning for the isolated synthetic pilot database.
-- Pass public_key from the operator-approved policy as a psql variable.
-- On an already provisioned pilot, run this file with -v review_fence_only=on.
-- This updates the review policy without reprovisioning the signed source.
\set ON_ERROR_STOP on
BEGIN;
\ir signed_fixture_grants.sql

-- Scheduled run IDs contain their pair and date separated by vertical bars.
-- Operator requests use the distinct manual:<UUID> namespace, including
-- before the durable receipt is populated. This also works on older schemas.
DROP POLICY IF EXISTS signed_pilot_read_only_reruns ON custody_runs;
CREATE POLICY signed_pilot_read_only_reruns ON custody_runs AS RESTRICTIVE
    FOR ALL TO PUBLIC USING (true) WITH CHECK (
        tenant_id <> 'synthetic-custody-pilot' OR
        NOT (starts_with(run_id, 'manual:') AND strpos(run_id, '|') = 0)
    );
COMMIT;
