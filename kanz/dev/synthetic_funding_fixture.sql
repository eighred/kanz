-- Fixture-only opening balances, readback role and failure injection. The
-- synthetic runner invokes this on its disposable database before enrollment tests.
BEGIN;
DO $$
DECLARE tenant text; opened_ns bigint; actor name;
BEGIN
    IF current_database() <> 'kanz_synthetic_oms' THEN
        RAISE EXCEPTION 'unexpected fixture database';
    END IF;
    FOR actor IN SELECT rolname FROM pg_roles WHERE rolname IN (
        'kanz_synthetic_oms_runtime','kanz_synthetic_oms_runtime_g2','kanz_synthetic_oms_runtime_g3',
        'kanz_synthetic_oms_adapter','kanz_synthetic_oms_adapter_g2','kanz_synthetic_oms_adapter_g3',
        'kanz_synthetic_oms_control','kanz_synthetic_oms_funding') LOOP
        IF pg_has_role(actor,'kanz_synthetic_funding_guard','MEMBER')
           OR has_function_privilege(actor,'synthetic_oms_reserve_authorized()','EXECUTE')
           OR EXISTS (SELECT FROM pg_class WHERE relnamespace='public'::regnamespace
              AND starts_with(relname,'capital_') AND relkind='r' AND
              (has_any_column_privilege(actor,oid,'SELECT,INSERT,UPDATE,REFERENCES')
               OR has_table_privilege(actor,oid,'DELETE,TRUNCATE,TRIGGER')))
           OR has_table_privilege(actor,'synthetic_oms_funding_authorizations','UPDATE,DELETE,TRUNCATE,TRIGGER')
           OR has_any_column_privilege(actor,'synthetic_oms_funding_authorizations','UPDATE')
           OR has_any_column_privilege(actor,'synthetic_oms_funding_authorizations','INSERT')
                IS DISTINCT FROM (actor='kanz_synthetic_oms_funding') THEN
            RAISE EXCEPTION 'unexpected funding authority for %',actor;
        END IF;
    END LOOP;
    opened_ns := (extract(epoch FROM clock_timestamp()) * 1000000000)::bigint;
    FOREACH tenant IN ARRAY ARRAY['synthetic-funded-ok','synthetic-funded-short',
        'synthetic-funded-race','synthetic-funded-rollback','synthetic-funded-foreign',
        'synthetic-funded-stale','synthetic-funded-short-fee'] LOOP
        PERFORM set_config('app.tenant_id',tenant,true);
        INSERT INTO capital_activation(enabled,activated_at) VALUES(true,clock_timestamp());
        INSERT INTO capital_source_health(tenant_id) VALUES(tenant);
        INSERT INTO capital_balances(portfolio_id,currency,revision,coverage_complete,total,observed_at_ns)
        VALUES ('synthetic-portfolio','USD',1,true,
                CASE tenant WHEN 'synthetic-funded-short' THEN '99'
                            WHEN 'synthetic-funded-race' THEN '150' ELSE '1000' END,opened_ns),
               ('synthetic-portfolio','BNB',1,true,
                CASE WHEN tenant='synthetic-funded-short-fee' THEN '0' ELSE '8' END,opened_ns);
    END LOOP;
END $$;

GRANT SELECT ON capital_balances,capital_commitments,capital_members,orders,
    synthetic_oms_commands,synthetic_oms_observations,outbox TO kanz_synthetic_oms_proof;

-- A nontransactional sequence witnesses that both reservations and the new
-- order actually existed at the point the outbox write was refused. A retry
-- can then commit against the very same cash and authorization.
CREATE ROLE kanz_synthetic_funding_probe NOLOGIN NOSUPERUSER NOBYPASSRLS;
-- The proof connection holds the second currency lock until both native
-- clients are visible in the lock graph. It has no direct balance-write grant.
GRANT SELECT, UPDATE(tenant_id) ON capital_balances TO kanz_synthetic_funding_probe;
CREATE FUNCTION synthetic_hold_competing_cash() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF app_current_tenant() IS DISTINCT FROM 'synthetic-funded-race' THEN
        RAISE EXCEPTION 'unexpected competition tenant';
    END IF;
    PERFORM 1 FROM public.capital_balances
        WHERE portfolio_id='synthetic-portfolio' AND currency='USD' FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'missing competition cash';
    END IF;
END $$;
ALTER FUNCTION synthetic_hold_competing_cash() OWNER TO kanz_synthetic_funding_probe;
REVOKE ALL ON FUNCTION synthetic_hold_competing_cash() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION synthetic_hold_competing_cash() TO kanz_synthetic_oms_proof;

CREATE SEQUENCE synthetic_funded_rollback_seen;
GRANT SELECT ON SEQUENCE synthetic_funded_rollback_seen TO kanz_synthetic_oms_proof;
GRANT USAGE ON SEQUENCE synthetic_funded_rollback_seen TO kanz_synthetic_funding_probe;
GRANT SELECT ON capital_commitments,capital_members,orders,synthetic_oms_commands
    TO kanz_synthetic_funding_probe;
CREATE FUNCTION synthetic_reject_funded_outbox() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF (SELECT count(*) FROM public.capital_commitments WHERE order_id=NEW.partition_key)=2
       AND (SELECT count(*) FROM public.capital_members WHERE order_id=NEW.partition_key)=2
       AND EXISTS (SELECT FROM public.orders WHERE order_id=NEW.partition_key)
       AND EXISTS (SELECT FROM public.synthetic_oms_commands WHERE order_id=NEW.partition_key) THEN
        IF nextval('public.synthetic_funded_rollback_seen')=1 THEN
            RAISE EXCEPTION 'injected failure after funding and order writes';
        END IF;
    END IF;
    RETURN NEW;
END $$;
ALTER FUNCTION synthetic_reject_funded_outbox() OWNER TO kanz_synthetic_funding_probe;
REVOKE ALL ON FUNCTION synthetic_reject_funded_outbox() FROM PUBLIC;
CREATE TRIGGER synthetic_funded_outbox_failure BEFORE INSERT ON outbox
    FOR EACH ROW WHEN (NEW.tenant_id='synthetic-funded-rollback')
    EXECUTE FUNCTION synthetic_reject_funded_outbox();
COMMIT;
