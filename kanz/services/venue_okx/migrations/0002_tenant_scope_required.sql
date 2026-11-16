-- AN UNSCOPED QUERY MUST ERROR, NEVER RETURN EMPTY (MT-01e).
--
-- Every tenant-scoped table on this platform enforced isolation with
--
--     USING (tenant_id = current_setting('app.tenant_id', true))
--
-- and that `true` is `missing_ok`. So a session that never set the GUC did not get
-- an error — it got NULL, the predicate went NULL, and the query returned ZERO ROWS.
-- Silently. An unscoped read and a tenant with genuinely no data are the SAME
-- OBSERVABLE EVENT, and the more dangerous one is invisible.
--
-- This is not hypothetical. The accounting service shipped with a pool that never set
-- the GUC: its ledger reads returned nothing, its writes failed, and every test was
-- green. It was found by executing the artifact, not by reading it.
--
-- app_current_tenant() RAISES instead. The rules it draws:
--
--   · no GUC, or an empty one   → ERROR (42501). The query cannot be answered,
--                                 because nobody said who is asking.
--   · a GUC, and no matching rows → 0 rows. That is not an error: it is isolation
--                                 working, and a tenant with no data is a real answer.
--
-- It is enforced AT THE ENGINE, which is the whole point: no Go code path, present or
-- future, can bypass it by forgetting a pool option. Verified against Postgres 16 —
-- the raise fires even when the table is EMPTY (the planner evaluates the STABLE
-- function once at executor init), so the guarantee does not depend on data existing.
--
-- Superusers still bypass RLS entirely. That is why the app role must be a
-- non-superuser; this migration hardens the check, it does not replace that rule.
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $fn$
DECLARE t text;
BEGIN
  t := current_setting('app.tenant_id', true);
  IF t IS NULL OR t = '' THEN
    RAISE EXCEPTION 'kanz: tenant scope missing — app.tenant_id is not set on this session'
      USING ERRCODE = '42501',
            HINT = 'every connection must set app.tenant_id (pgxpool AfterConnect); see internal/pg.NewTenantPool';
  END IF;
  RETURN t;
END $fn$;

-- THIS MIGRATION TOUCHES VENUE-OKX'S OWN TABLES AND NOTHING ELSE (#227).
--
-- It used to discover its targets: `SELECT relname FROM pg_class WHERE relrowsecurity
-- AND relnamespace = current_schema()`, then drop and recreate every policy it found.
-- Eight services shipped a byte-identical copy of that loop. In a schema holding more
-- than one service's tables, whichever copy ran LAST rewrote all of them — including
-- policies that were deliberately STRONGER than the generic one below.
--
-- Reproduced against Postgres 16 with the real migration files: after accounting/0003
-- installed
--
--     WITH CHECK (tenant_id = app_current_tenant()
--             AND venue_account_id = app_current_venue_account())
--
-- wealth's copy of this loop reduced it to `WITH CHECK (tenant_id = app_current_tenant())`.
-- Both wealth migrations reported success. No error, no warning, no notice. The
-- collateral-segregation guard that accounting/0003 exists to enforce — the one keeping
-- one portfolio's margin from being liquidated for another's drawdown — was gone, and
-- the only way to notice was to go looking in pg_policies afterwards.
--
-- A migration cannot know that a policy it did not write is weaker than its own. So it
-- does not get to decide: the table list is explicit, it is this service's, and a table
-- another service owns is not in it. Same FOREACH-over-a-literal-array shape the
-- CREATE TABLE migrations here already use.
--
-- Policies on THESE tables are still dropped wholesale rather than by name: a leftover
-- permissive policy would be OR'd with this one and would quietly restore the silent
-- empty read. That enumeration is scoped to a named table and is not discovery.
DO $$
DECLARE
  t text;
  p record;
BEGIN
  FOREACH t IN ARRAY ARRAY['venue_orders'] LOOP
    FOR p IN
      SELECT policyname FROM pg_policies
      WHERE schemaname = current_schema() AND tablename = t
    LOOP
      EXECUTE format('DROP POLICY %I ON %I', p.policyname, t);
    END LOOP;

    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = app_current_tenant())
        WITH CHECK (tenant_id = app_current_tenant())
    $f$, t);

    IF EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = current_schema() AND table_name = t AND column_name = 'tenant_id'
    ) THEN
      EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant_id SET DEFAULT app_current_tenant()', t);
    END IF;
  END LOOP;
END $$;
