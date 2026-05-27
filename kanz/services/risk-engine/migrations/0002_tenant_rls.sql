-- 0002: tenant isolation via row-level security (MT-01d).
--
-- State isolation is the costly-to-retrofit half of multi-tenancy: a query that
-- forgets a `WHERE tenant_id =` clause must not be able to leak across tenants.
-- RLS makes the tenant boundary a property of the DATABASE, not a convention the
-- app remembers. The session GUC `app.tenant_id` carries the authenticated
-- session's tenant (set by the engine's pool from its configured tenant — the
-- "single-tenant-now" deployment, MT-01b); RLS scopes every read/write to it.
--
-- Deny-by-default (matching AUTH-01b): a connection with `app.tenant_id` UNSET
-- sees no rows and can write none — current_setting(...,true) is NULL, so the
-- policy predicate is NULL ⇒ fail-closed. Superusers bypass RLS, so the engine
-- must connect as a non-superuser role.

-- 1. Tenant column on every stateful table. Existing rows backfill to the
--    reserved __system__ tenant (MT-01a: pre-tenancy state is __system__).
ALTER TABLE portfolios   ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '__system__';
ALTER TABLE positions    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '__system__';
ALTER TABLE applied_keys ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '__system__';

-- 2. Tenant becomes part of identity, so the same portfolio_id can exist in
--    different tenants without colliding. Rebuild PKs and the child FKs.
ALTER TABLE positions    DROP CONSTRAINT positions_portfolio_id_fkey;
ALTER TABLE applied_keys DROP CONSTRAINT applied_keys_portfolio_id_fkey;

ALTER TABLE portfolios   DROP CONSTRAINT portfolios_pkey;
ALTER TABLE portfolios   ADD  PRIMARY KEY (tenant_id, portfolio_id);

ALTER TABLE positions    DROP CONSTRAINT positions_pkey;
ALTER TABLE positions    ADD  PRIMARY KEY (tenant_id, portfolio_id, instrument_id);
ALTER TABLE positions    ADD  FOREIGN KEY (tenant_id, portfolio_id)
                              REFERENCES portfolios (tenant_id, portfolio_id) ON DELETE CASCADE;

ALTER TABLE applied_keys DROP CONSTRAINT applied_keys_pkey;
ALTER TABLE applied_keys ADD  PRIMARY KEY (tenant_id, portfolio_id, idempotency_key);
ALTER TABLE applied_keys ADD  FOREIGN KEY (tenant_id, portfolio_id)
                              REFERENCES portfolios (tenant_id, portfolio_id) ON DELETE CASCADE;

-- The applied-keys pruning index gains the tenant lead (the writer prunes
-- within a tenant's portfolio).
DROP INDEX applied_keys_portfolio_applied_at_idx;
CREATE INDEX applied_keys_tenant_portfolio_applied_at_idx
    ON applied_keys (tenant_id, portfolio_id, applied_at);

-- 3. New inserts stamp the session tenant. The persist layer writes it
--    explicitly via current_setting('app.tenant_id') (the primary path); this
--    default is the safety net for any direct insert. The WITH CHECK policy
--    below rejects a row whose tenant_id differs from the session.
ALTER TABLE portfolios   ALTER COLUMN tenant_id SET DEFAULT current_setting('app.tenant_id', true);
ALTER TABLE positions    ALTER COLUMN tenant_id SET DEFAULT current_setting('app.tenant_id', true);
ALTER TABLE applied_keys ALTER COLUMN tenant_id SET DEFAULT current_setting('app.tenant_id', true);

-- 4. Enable + FORCE RLS and a deny-by-default isolation policy on each table.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['portfolios', 'positions', 'applied_keys'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE  ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = current_setting('app.tenant_id', true))
        WITH CHECK (tenant_id = current_setting('app.tenant_id', true))
    $f$, t);
  END LOOP;
END $$;
