-- Enable only on the disposable synthetic estate after funding.sql.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
  IF current_database()<>'kanz_synthetic_oms' THEN RAISE EXCEPTION 'isolated Go admission required'; END IF;
END $$;

-- The original unfunded-only installation can be extended in place. Refuse
-- an unfamiliar owner, function body, disabled/deferred trigger or partial
-- installation. Existing command bytes and the trigger/function OIDs survive.
SELECT to_regprocedure('public.synthetic_oms_check_go_admission()') IS NULL AS fresh \gset
\if :fresh
-- Only the independent verifier can record the broker envelope digest and
-- idempotency key on its entitlement decision. Legacy inputs remain unknown.
ALTER TABLE public.synthetic_oms_evidence
    ADD COLUMN gateway_frame_sha256 bytea,
    ADD COLUMN gateway_idempotency_key text,
    ADD CHECK ((gateway_frame_sha256 IS NULL)=(gateway_idempotency_key IS NULL)
      AND (gateway_frame_sha256 IS NULL OR (kind='entitlement' AND octet_length(gateway_frame_sha256)=32
           AND octet_length(gateway_idempotency_key) BETWEEN 1 AND 256)));
ALTER TABLE public.synthetic_oms_commands ADD COLUMN go_gateway_frame bytea
    CHECK (octet_length(go_gateway_frame) BETWEEN 1 AND 131072);

CREATE ROLE kanz_synthetic_go_admission_guard NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_assert_writer() TO kanz_synthetic_go_admission_guard;
GRANT SELECT ON public.synthetic_oms_evidence, public.orders, public.outbox TO kanz_synthetic_go_admission_guard;
GRANT SELECT(tenant_id,order_id) ON public.synthetic_oms_funding_authorizations,public.capital_members TO kanz_synthetic_go_admission_guard;
GRANT SELECT, UPDATE(tenant_id) ON public.capital_activation TO kanz_synthetic_go_admission_guard;

\else
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_proc p JOIN pg_roles r ON r.oid=p.proowner
        JOIN pg_trigger t ON t.tgfoid=p.oid
        WHERE p.oid='public.synthetic_oms_check_go_admission()'::regprocedure AND p.prosecdef
          AND r.rolname='kanz_synthetic_go_admission_guard' AND NOT (r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreaterole)
          AND encode(sha256(convert_to(replace(p.prosrc,E'\r\n',E'\n'),'UTF8')),'hex')='a596394695e255a9175f19de857abb788cdc775247bc9e789a6290b25d42400a'
          AND p.proconfig=ARRAY['search_path=pg_catalog, public, pg_temp']
          AND t.tgname='synthetic_go_admission' AND t.tgrelid='public.synthetic_oms_commands'::regclass
          AND t.tgdeferrable AND t.tginitdeferred AND t.tgenabled='O'
          AND pg_get_triggerdef(t.oid)='CREATE CONSTRAINT TRIGGER synthetic_go_admission AFTER INSERT ON public.synthetic_oms_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN ((new.go_gateway_frame IS NOT NULL)) EXECUTE FUNCTION synthetic_oms_check_go_admission()') THEN
        RAISE EXCEPTION 'unrecognized retained Go admission guard';
    END IF;
END $$;
\endif

ALTER TABLE public.synthetic_oms_commands ADD COLUMN go_funding_amounts text[]
  CHECK (go_funding_amounts IS NULL OR (go_gateway_frame IS NOT NULL AND
    array_ndims(go_funding_amounts)=1 AND array_lower(go_funding_amounts,1)=1 AND
    cardinality(go_funding_amounts)=2 AND array_position(go_funding_amounts,NULL) IS NULL));
GRANT SELECT ON public.synthetic_oms_funding_authorizations,public.capital_members,public.capital_commitments
    TO kanz_synthetic_go_admission_guard;
GRANT SELECT(tenant_id,portfolio_id,currency,observed_at_ns) ON public.capital_balances TO kanz_synthetic_go_admission_guard;

CREATE OR REPLACE FUNCTION public.synthetic_oms_check_go_admission() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE source_event text; trace text; sources text[]; checked_at timestamptz; armed boolean;
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM public.app_current_tenant() OR NEW.phase<>'accepted'
       OR NEW.writer_epoch IS DISTINCT FROM public.synthetic_oms_assert_writer()
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER')
       OR sha256(NEW.command)<>NEW.command_sha256 THEN RAISE EXCEPTION 'invalid Go admission scope'; END IF;
    -- Lock a preexisting posture. A concurrent activation cannot turn a new
    -- unreserved Go order into a permissible admission (or vice versa).
    SELECT enabled INTO armed FROM public.capital_activation WHERE tenant_id=NEW.tenant_id AND xmin<>pg_current_xact_id()::xid FOR SHARE;
    IF NOT FOUND OR armed IS DISTINCT FROM (NEW.go_funding_amounts IS NOT NULL) THEN
        RAISE EXCEPTION 'Go handoff funding posture unknown or inconsistent'; END IF;
    checked_at:=clock_timestamp();
    IF NOT armed THEN
        IF EXISTS(SELECT FROM public.capital_members WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
           OR EXISTS(SELECT FROM public.synthetic_oms_funding_authorizations WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
           THEN RAISE EXCEPTION 'unfunded Go handoff has funding evidence'; END IF;
    ELSE
        -- The funding trigger is the sole reserver. At commit require the
        -- independent approval, both original cash memberships and exact fresh
        -- debits in this transaction, before retaining the Go acceptance.
        IF NOT EXISTS (SELECT FROM public.synthetic_oms_funding_authorizations a WHERE
            (a.tenant_id,a.order_id,a.principal_id,a.portfolio_id,a.command_sha256,a.venue,a.venue_account_id)=
            (NEW.tenant_id,NEW.order_id,NEW.principal_id,NEW.portfolio_id,NEW.command_sha256,'XSIM','synthetic-spot')
            AND a.currencies=ARRAY['BNB','USD'] AND a.amounts=NEW.go_funding_amounts AND a.expires_at>checked_at)
           OR (SELECT count(*) FROM public.capital_commitments WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))<>2
           OR (SELECT count(*) FROM public.capital_members WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))<>2
           OR (SELECT count(*) FROM unnest(ARRAY['BNB','USD'],NEW.go_funding_amounts) AS leg(currency,amount)
               JOIN public.capital_commitments c ON (c.tenant_id,c.order_id,c.portfolio_id,c.currency)=
                   (NEW.tenant_id,NEW.order_id,NEW.portfolio_id,leg.currency)
               JOIN public.capital_members m ON (m.tenant_id,m.order_id,m.currency)=(c.tenant_id,c.order_id,c.currency)
               JOIN public.capital_balances b ON (b.tenant_id,b.portfolio_id,b.currency)=(c.tenant_id,c.portfolio_id,c.currency)
               WHERE m.owner_order_id=NEW.order_id AND c.xmin=pg_current_xact_id()::xid AND m.xmin=pg_current_xact_id()::xid
                 AND c.required_debit=leg.amount AND c.admission_debit=leg.amount AND c.version=1
                 AND c.allocated_debit='0' AND m.allocated_debit='0' AND c.executed_debit='0' AND c.booked_debit='0'
                 AND m.executed_debit='0' AND m.booked_debit='0'
                 AND extract(epoch FROM checked_at)*1000000000-b.observed_at_ns BETWEEN 0 AND 60000000000)<>2 THEN
            RAISE EXCEPTION 'Go admission lacks its exact original funded reservation'; END IF;
    END IF;
    SELECT array_agg(source_id ORDER BY kind),
           max(source_id) FILTER(WHERE kind='entitlement'), max(trace_context) FILTER(WHERE kind='entitlement')
      INTO sources,source_event,trace
      FROM public.synthetic_oms_evidence
      WHERE (tenant_id,principal_id,portfolio_id,order_id,command_sha256,instrument_id)=
            (NEW.tenant_id,NEW.principal_id,NEW.portfolio_id,NEW.order_id,NEW.command_sha256,'SYNTH-USD')
        AND decision='ALLOW' AND effective_at<=observed_at AND observed_at<=checked_at AND expires_at>checked_at
        AND (kind<>'market' OR effective_at>=checked_at-interval '30 seconds')
        AND (kind<>'entitlement' OR (gateway_idempotency_key=NEW.idempotency_key AND gateway_frame_sha256=sha256(NEW.go_gateway_frame)));
    IF cardinality(sources) IS DISTINCT FROM 5 OR sources IS DISTINCT FROM NEW.evidence_ids THEN
        RAISE EXCEPTION 'missing effective Go admission evidence'; END IF;
    -- An order created by an earlier transaction cannot acquire invented
    -- provenance. The ordinary Go store must commit its own pending FACT.
    IF NOT EXISTS(SELECT FROM public.orders o WHERE (o.tenant_id,o.order_id,o.portfolio_id,o.status,o.version)=
            (NEW.tenant_id,NEW.order_id,NEW.portfolio_id,1,0) AND o.xmin=pg_current_xact_id()::xid)
       OR (SELECT count(*) FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(NEW.tenant_id,NEW.order_id))<>1
       OR NOT EXISTS(SELECT FROM public.outbox b WHERE
            (b.tenant_id,b.envelope_tenant_id,b.partition_key,b.producer_service,b.subject,b.event_type,b.event_class,
             b.schema_version,b.domain,b.payload_schema_ref,b.correlation_id,b.causation_id,b.trace_context)=
            (NEW.tenant_id,NEW.tenant_id,NEW.order_id,'oms','order.order.accepted','order.order.accepted',1,
             1,'order','order.v1.OrderAccepted:1',source_event,source_event,trace)
            AND b.published_at IS NULL AND b.attempts=0 AND b.xmin=pg_current_xact_id()::xid) THEN
        RAISE EXCEPTION 'Go admission lacks an atomic order and acceptance'; END IF;
    RETURN NULL;
END $$;
\if :fresh
ALTER FUNCTION public.synthetic_oms_check_go_admission() OWNER TO kanz_synthetic_go_admission_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_check_go_admission() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER synthetic_go_admission AFTER INSERT
    ON public.synthetic_oms_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.go_gateway_frame IS NOT NULL) EXECUTE FUNCTION public.synthetic_oms_check_go_admission();
\endif
COMMIT;
