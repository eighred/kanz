-- Enable only on the disposable synthetic estate after funding.sql.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
  IF current_database()<>'kanz_synthetic_oms' THEN RAISE EXCEPTION 'isolated Go admission required'; END IF;
END $$;

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

CREATE FUNCTION public.synthetic_oms_check_go_admission() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE source_event text; trace text; sources text[]; checked_at timestamptz;
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM public.app_current_tenant() OR NEW.phase<>'accepted'
       OR NEW.writer_epoch IS DISTINCT FROM public.synthetic_oms_assert_writer()
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER')
       OR sha256(NEW.command)<>NEW.command_sha256 THEN RAISE EXCEPTION 'invalid Go admission scope'; END IF;
    -- Require an explicitly disabled capital row. Serialize with activation;
    -- a missing posture, pre-existing reservation or funding proof refuses.
    PERFORM FROM public.capital_activation WHERE tenant_id=NEW.tenant_id AND NOT enabled AND xmin<>pg_current_xact_id()::xid FOR SHARE;
    IF NOT FOUND OR EXISTS(SELECT FROM public.capital_members WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
       OR EXISTS(SELECT FROM public.synthetic_oms_funding_authorizations WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
       THEN RAISE EXCEPTION 'Go handoff supports only explicitly unfunded orders'; END IF;
    checked_at:=clock_timestamp();
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
ALTER FUNCTION public.synthetic_oms_check_go_admission() OWNER TO kanz_synthetic_go_admission_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_check_go_admission() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER synthetic_go_admission AFTER INSERT
    ON public.synthetic_oms_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.go_gateway_frame IS NOT NULL) EXECUTE FUNCTION public.synthetic_oms_check_go_admission();
COMMIT;
