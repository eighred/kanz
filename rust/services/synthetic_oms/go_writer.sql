-- Install after writer.sql in the disposable estate. The marker role grants
-- no table rights. The existing handoff decides which login becomes active.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' THEN
        RAISE EXCEPTION 'Go writer handback requires its disposable database';
    END IF;
END $$;

CREATE ROLE kanz_synthetic_oms_go_runtime NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS;

CREATE FUNCTION public.synthetic_oms_check_go_handback() RETURNS trigger LANGUAGE plpgsql
    SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
BEGIN
    IF NOT pg_has_role(NEW.runtime_oid,'kanz_synthetic_oms_go_runtime','MEMBER') THEN
        RETURN NEW;
    END IF;
    -- The handoff holds the writer row exclusively. In a fixed snapshot a
    -- preceding writer can commit while the caller waits for that same lock.
    IF current_setting('transaction_isolation')<>'read committed' THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='Go handback requires read committed';
    END IF;

    -- The Go restart sweep may send PENDING_NEW and query ROUTED. Transfer
    -- only the qualified routed case, with no funding, fill or cancel in flight.
    IF EXISTS (
        SELECT FROM public.synthetic_oms_commands c
        JOIN public.orders o USING(tenant_id,order_id)
        WHERE c.phase<>'working' OR o.status<>2
           OR o.portfolio_id IS DISTINCT FROM c.portfolio_id
           OR NOT EXISTS (
              SELECT FROM public.synthetic_oms_observations a
              WHERE (a.tenant_id,a.order_id)=(c.tenant_id,c.order_id)
                AND a.operation='ack' AND a.observation=convert_to(c.order_id,'UTF8'))
           OR NOT EXISTS (
              SELECT FROM public.synthetic_oms_observations d
              WHERE (d.tenant_id,d.order_id)=(c.tenant_id,c.order_id)
                AND d.operation='dispatch' AND d.observation=''::bytea)
           OR EXISTS (
              SELECT FROM public.synthetic_oms_observations x
              WHERE (x.tenant_id,x.order_id)=(c.tenant_id,c.order_id)
                AND x.operation NOT IN ('dispatch','timeout','ack'))
           OR EXISTS (SELECT FROM public.order_fills f
                      WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id))
           OR EXISTS (SELECT FROM public.order_executions f
                      WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id))
           OR EXISTS (SELECT FROM public.capital_commitments f
                      WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id))
           OR EXISTS (SELECT FROM public.capital_members f
                      WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id))
           OR EXISTS (SELECT FROM public.capital_activation f WHERE f.tenant_id=c.tenant_id AND f.enabled)
    ) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='retained order is not ready for Go recovery';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION public.synthetic_oms_check_go_handback() FROM PUBLIC;
CREATE TRIGGER require_go_handback BEFORE UPDATE OF runtime_oid ON public.synthetic_oms_writer
    FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_check_go_handback();
COMMIT;
