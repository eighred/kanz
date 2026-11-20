-- Install after schema.sql. Take the writer locks and install all guards in
-- one transaction so earlier clients cannot see a partially fenced estate.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' THEN
        RAISE EXCEPTION 'synthetic writer handoff requires its disposable database';
    END IF;
END $$;
-- A dedicated synthetic estate has one financial writer generation. The OIDs
-- are authenticated PostgreSQL login identities, independent of any client input.
CREATE TABLE public.synthetic_oms_writer (
    singleton boolean PRIMARY KEY CHECK (singleton),
    epoch bigint NOT NULL CHECK (epoch >= 0),
    runtime_oid oid,
    adapter_oid oid,
    CHECK ((epoch=0 AND runtime_oid IS NULL AND adapter_oid IS NULL)
        OR (epoch>0 AND runtime_oid IS NOT NULL AND adapter_oid IS NOT NULL AND runtime_oid<>adapter_oid))
);
INSERT INTO synthetic_oms_writer(singleton,epoch) VALUES(true,0);
CREATE TABLE public.synthetic_oms_writer_history (
    epoch bigint PRIMARY KEY CHECK (epoch>0),
    runtime_oid oid NOT NULL UNIQUE,
    runtime_name name NOT NULL UNIQUE,
    adapter_oid oid NOT NULL UNIQUE,
    adapter_name name NOT NULL UNIQUE,
    switched_by name NOT NULL DEFAULT session_user,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
REVOKE ALL ON synthetic_oms_writer,synthetic_oms_writer_history FROM PUBLIC;

CREATE FUNCTION public.synthetic_oms_handoff(expected bigint, next_runtime regrole, next_adapter regrole)
RETURNS bigint LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
DECLARE old_epoch bigint; app_name name; venue_name name;
BEGIN
    SELECT epoch INTO STRICT old_epoch FROM public.synthetic_oms_writer WHERE singleton FOR UPDATE;
    IF (SELECT count(*) FROM public.synthetic_oms_writer_history)<>old_epoch OR
       (old_epoch>0 AND NOT EXISTS(SELECT FROM public.synthetic_oms_writer w
          JOIN public.synthetic_oms_writer_history h USING(epoch)
          WHERE w.singleton AND h.runtime_oid=w.runtime_oid AND h.adapter_oid=w.adapter_oid)) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='incomplete writer history';
    END IF;
    IF expected IS DISTINCT FROM old_epoch THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='stale writer handoff';
    END IF;
    SELECT rolname INTO STRICT app_name FROM pg_roles
        WHERE oid=next_runtime AND rolcanlogin AND NOT (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication)
          AND pg_has_role(oid,'kanz_synthetic_oms_runtime','USAGE')
          AND NOT pg_has_role(oid,'kanz_synthetic_oms_adapter','MEMBER');
    SELECT rolname INTO STRICT venue_name FROM pg_roles
        WHERE oid=next_adapter AND rolcanlogin AND NOT (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication)
          AND pg_has_role(oid,'kanz_synthetic_oms_adapter','USAGE')
          AND NOT pg_has_role(oid,'kanz_synthetic_oms_runtime','MEMBER');
    IF EXISTS (SELECT FROM public.synthetic_oms_writer_history
               WHERE runtime_oid IN(next_runtime,next_adapter) OR adapter_oid IN(next_runtime,next_adapter)
                  OR runtime_name IN(app_name,venue_name) OR adapter_name IN(app_name,venue_name)) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='retired writer identity';
    END IF;
    -- Empty generation zero is deliberately unowned. Never admit an unbound
    -- legacy aggregate, journal entry or pending FACT by inferring its owner.
    IF (old_epoch=0 AND EXISTS(SELECT FROM public.synthetic_oms_commands))
       OR EXISTS(SELECT FROM public.synthetic_oms_commands WHERE writer_epoch IS NULL)
       OR EXISTS(SELECT FROM public.synthetic_oms_observations WHERE writer_epoch IS NULL)
       OR EXISTS(SELECT FROM public.orders o FULL JOIN public.synthetic_oms_commands c
                 USING(tenant_id,order_id) WHERE o.order_id IS NULL OR c.order_id IS NULL)
       OR EXISTS(SELECT FROM public.order_fills f WHERE NOT EXISTS
                (SELECT FROM public.synthetic_oms_commands c WHERE (c.tenant_id,c.order_id)=(f.tenant_id,f.order_id)))
       OR EXISTS(SELECT FROM public.order_executions f WHERE NOT EXISTS
                (SELECT FROM public.synthetic_oms_commands c WHERE (c.tenant_id,c.order_id)=(f.tenant_id,f.order_id)))
       OR EXISTS(SELECT FROM public.outbox f WHERE f.producer_service IS DISTINCT FROM 'oms' OR NOT EXISTS
                (SELECT FROM public.synthetic_oms_commands c WHERE (c.tenant_id,c.order_id)=(f.tenant_id,f.partition_key))) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='unowned retained economic state';
    END IF;
    INSERT INTO public.synthetic_oms_writer_history(epoch,runtime_oid,runtime_name,adapter_oid,adapter_name)
        VALUES(old_epoch+1,next_runtime,app_name,next_adapter,venue_name);
    UPDATE public.synthetic_oms_writer SET epoch=old_epoch+1,runtime_oid=next_runtime,adapter_oid=next_adapter WHERE singleton;
    RETURN old_epoch+1;
END $$;
REVOKE ALL ON FUNCTION synthetic_oms_handoff(bigint,regrole,regrole) FROM PUBLIC;

CREATE FUNCTION public.synthetic_oms_assert_writer() RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE owner public.synthetic_oms_writer%ROWTYPE;
BEGIN
    SELECT * INTO STRICT owner FROM public.synthetic_oms_writer WHERE singleton FOR SHARE;
    IF owner.epoch=0 OR session_user::regrole::oid NOT IN (owner.runtime_oid,owner.adapter_oid)
       OR NOT EXISTS(SELECT FROM public.synthetic_oms_writer_history h WHERE h.epoch=owner.epoch
            AND h.runtime_oid=owner.runtime_oid AND h.adapter_oid=owner.adapter_oid) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='unowned writer generation';
    END IF;
    RETURN owner.epoch;
END $$;
REVOKE ALL ON FUNCTION synthetic_oms_assert_writer() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION synthetic_oms_assert_writer() TO kanz_synthetic_oms_runtime,kanz_synthetic_oms_adapter;

CREATE FUNCTION public.synthetic_oms_fence_mutation() RETURNS trigger LANGUAGE plpgsql
    SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF TG_TABLE_NAME IN ('synthetic_oms_commands','synthetic_oms_observations') THEN
        IF TG_OP='INSERT' THEN
            NEW.writer_epoch := public.synthetic_oms_assert_writer();
        ELSE
            PERFORM public.synthetic_oms_assert_writer();
            NEW.writer_epoch := OLD.writer_epoch;
        END IF;
    ELSE
        PERFORM public.synthetic_oms_assert_writer();
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION synthetic_oms_fence_mutation() FROM PUBLIC;
DO $$
DECLARE tab text;
BEGIN
    FOREACH tab IN ARRAY ARRAY['synthetic_oms_commands','synthetic_oms_observations',
            'orders','order_fills','order_executions'] LOOP
        EXECUTE format('CREATE TRIGGER require_synthetic_writer BEFORE INSERT OR UPDATE ON public.%I
                        FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_fence_mutation()',tab);
    END LOOP;
    -- The relay retains its separate mark-published authority on existing rows.
    CREATE TRIGGER require_synthetic_writer BEFORE INSERT ON public.outbox
        FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_fence_mutation();
END $$;
-- Existing records retain NULL as their unknown pre-fence provenance.
-- NOT VALID preserves those rows while PostgreSQL checks every new write.
ALTER TABLE public.synthetic_oms_commands ADD COLUMN writer_epoch bigint REFERENCES public.synthetic_oms_writer_history(epoch),
    ADD CONSTRAINT synthetic_command_has_writer CHECK(writer_epoch IS NOT NULL) NOT VALID;
ALTER TABLE public.synthetic_oms_observations ADD COLUMN writer_epoch bigint REFERENCES public.synthetic_oms_writer_history(epoch),
    ADD CONSTRAINT synthetic_observation_has_writer CHECK(writer_epoch IS NOT NULL) NOT VALID;
COMMIT;
