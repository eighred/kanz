-- Staged #1747 parameter-administrator prerequisite, separate from table owner
-- authority. Supply the independently verified runtime_role to psql as in the
-- bookkeeping template; this does not rotate credentials or alter live defaults.
\set ON_ERROR_STOP on
\if :{?runtime_role}
\else
\echo 'runtime_role must be explicitly supplied and verified'
\quit 2
\endif
BEGIN;
SET LOCAL search_path=pg_catalog;
SELECT pg_catalog.set_config('kanz.hardening_runtime_role', :'runtime_role', true);
DO $$
DECLARE runtime_name text := pg_catalog.current_setting('kanz.hardening_runtime_role'); runtime_oid oid;
BEGIN
    SELECT oid INTO runtime_oid FROM pg_catalog.pg_roles
      WHERE rolname=runtime_name AND rolcanlogin AND NOT rolsuper AND NOT rolbypassrls;
    IF runtime_oid IS NULL OR runtime_name IN (current_user,session_user) THEN
        RAISE EXCEPTION 'independent restricted runtime role is required';
    END IF;
    EXECUTE pg_catalog.format('REVOKE SET, ALTER SYSTEM ON PARAMETER session_replication_role FROM %I, PUBLIC', runtime_name);
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE pg_catalog.pg_has_role(runtime_oid,r.oid,'MEMBER') AND
       pg_catalog.has_parameter_privilege(r.oid,'session_replication_role','SET,ALTER SYSTEM')) THEN
        RAISE EXCEPTION 'effective replication-parameter authority remains; shared-role repair requires an explicit administrator decision';
    END IF;
END $$;
COMMIT;
-- Separately inspect server, database and role defaults, including per-database
-- role settings; reconnect as the actual runtime and require origin/local.
-- A revoke does not repair an existing replica session or such defaults.
