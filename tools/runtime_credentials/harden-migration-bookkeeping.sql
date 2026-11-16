-- Staged #1747 owner-side prerequisite; never invoked by an active workload.
-- psql --set=ON_ERROR_STOP=1 --set=runtime_role=EXPLICIT_VERIFIED_ROLE --file=...
-- Role names are non-secret. Authenticate through the existing controlled
-- owner-side credential mechanism; never put a DSN/password in argv or a file.
\set ON_ERROR_STOP on
\if :{?runtime_role}
\else
\echo 'runtime_role must be explicitly supplied and verified for this database'
\quit 2
\endif
BEGIN;
SELECT set_config('kanz.hardening_runtime_role', :'runtime_role', true);
DO $$
DECLARE
    runtime_name text := current_setting('kanz.hardening_runtime_role');
    runtime_oid oid;
    bookkeeping oid := to_regclass('public.schema_migrations');
    column_name text;
BEGIN
    SELECT oid INTO runtime_oid FROM pg_roles
      WHERE rolname=runtime_name AND rolcanlogin AND NOT rolsuper AND NOT rolbypassrls;
    IF runtime_oid IS NULL OR bookkeeping IS NULL THEN
        RAISE EXCEPTION 'actual restricted runtime role and migrated bookkeeping table are required';
    END IF;
    IF NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid=bookkeeping), 'MEMBER') OR
       pg_has_role(runtime_oid, (SELECT relowner FROM pg_class WHERE oid=bookkeeping), 'MEMBER') THEN
        RAISE EXCEPTION 'authorized owner and independent runtime role are required';
    END IF;
    EXECUTE format('REVOKE INSERT, UPDATE, DELETE ON TABLE public.schema_migrations FROM %I, PUBLIC', runtime_name);
    FOR column_name IN SELECT attname FROM pg_attribute WHERE attrelid=bookkeeping AND attnum>0 AND NOT attisdropped LOOP
        EXECUTE format('REVOKE INSERT (%I), UPDATE (%I) ON TABLE public.schema_migrations FROM %I, PUBLIC', column_name, column_name, runtime_name);
    END LOOP;
    -- An inherited/assumable group grant needs its own explicit owner decision.
    -- Do not silently revoke a shared group's unrelated authority.
    IF EXISTS (SELECT 1 FROM pg_roles r WHERE pg_has_role(runtime_oid,r.oid,'MEMBER') AND
       (has_table_privilege(r.oid,bookkeeping,'INSERT,UPDATE,DELETE') OR has_any_column_privilege(r.oid,bookkeeping,'INSERT,UPDATE'))) THEN
        RAISE EXCEPTION 'effective migration-bookkeeping writes remain through another role; transaction refused';
    END IF;
END $$;
COMMIT;
