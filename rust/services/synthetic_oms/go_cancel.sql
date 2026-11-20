-- Opt in after settlement.sql, go_admission.sql and go_writer.sql. The
-- working Go login receives two operations; the accounting kernel, generic
-- adapter entry point and evidence issuer remain separate authorities.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' THEN RAISE EXCEPTION 'isolated Go cancellation required'; END IF;
END $$;

GRANT SELECT(go_gateway_frame,go_funding_amounts,writer_epoch) ON public.synthetic_oms_commands TO kanz_synthetic_settlement_guard;
GRANT SELECT(writer_epoch,observation) ON public.synthetic_oms_observations TO kanz_synthetic_settlement_guard;

CREATE FUNCTION public.synthetic_oms_go_cancel(
    finish boolean,p_portfolio text,p_order text,p_principal text,p_event text,p_trace text,p_seconds bigint,p_nanos integer,p_cancel bytea,
    terminal_event text,terminal_seconds bigint,terminal_nanos integer,body bytea,next_state bytea,fact bytea)
RETURNS text LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
    tenant text:=public.app_current_tenant(); epoch bigint; phase text; status integer; claimed boolean;
    result text; version bigint; state_sha bytea; command_sha bytea;
BEGIN
    IF NOT pg_has_role(session_user,'kanz_synthetic_oms_go_working_runtime','USAGE')
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER')
       OR current_setting('transaction_isolation')<>'read committed'
       OR p_cancel IS NULL OR octet_length(p_cancel) NOT BETWEEN 1 AND 65536
       OR p_seconds IS NULL OR p_seconds NOT BETWEEN -62135596800 AND 253402300799
       OR p_nanos IS NULL OR p_nanos NOT BETWEEN 0 AND 999999999 THEN RETURN 'HELD'; END IF;
    epoch:=public.synthetic_oms_assert_writer();
    -- Same lock as the Rust adapter, ahead of command, aggregate and cash
    -- reads. Re-read on acquiring it: no concurrent caller can also get SEND.
    PERFORM pg_advisory_xact_lock(hashtextextended(json_build_array(tenant,p_order)::text,1532));
    SELECT c.phase,o.status,o.version,sha256(o.state),c.command_sha256 INTO phase,status,version,state_sha,command_sha
      FROM public.synthetic_oms_commands c
      JOIN public.orders o USING(tenant_id,order_id,portfolio_id)
      JOIN public.synthetic_oms_funding_authorizations f USING(tenant_id,order_id,portfolio_id,principal_id,command_sha256)
      JOIN public.synthetic_oms_cancellation_authorizations a USING(tenant_id,order_id,portfolio_id,principal_id)
      JOIN public.synthetic_oms_settlement_authorizations permit
        ON (permit.tenant_id,permit.order_id,permit.portfolio_id,permit.principal_id,permit.command_sha256)=
           (c.tenant_id,c.order_id,c.portfolio_id,c.principal_id,c.command_sha256)
      WHERE (c.tenant_id,c.order_id,c.portfolio_id,c.principal_id)=(tenant,p_order,p_portfolio,p_principal)
        AND c.go_gateway_frame IS NOT NULL AND c.writer_epoch<epoch AND c.go_funding_amounts=f.amounts
        AND f.currencies=ARRAY['BNB','USD'] AND (f.venue,f.venue_account_id)=('XSIM','synthetic-spot')
        AND o.parent_order_id='' AND o.allocated_quantity=0
        AND (a.event_id,a.trace_context,a.command_sha256)=(p_event,p_trace,sha256(p_cancel))
        AND a.effective_at=to_timestamp(p_seconds)+(p_nanos/1000)*interval '1 microsecond'
        AND (permit.writer_epoch,permit.event_id,permit.operation,permit.effective_seconds,permit.effective_nanos)=
            (epoch,p_event,'cancel_pending',p_seconds,p_nanos)
        AND EXISTS (SELECT FROM public.synthetic_oms_observations fill
               JOIN public.synthetic_oms_settlement_authorizations observed USING(tenant_id,order_id,event_id)
               JOIN public.order_executions j ON (j.tenant_id,j.order_id,j.fill_id)=(fill.tenant_id,fill.order_id,fill.event_id)
               WHERE (fill.tenant_id,fill.order_id,fill.operation,fill.writer_epoch)=(tenant,p_order,'fill',epoch-1)
                 AND observed.operation='fill' AND observed.writer_epoch=fill.writer_epoch AND observed.command_sha256=c.command_sha256
                 AND observed.venue_terms_sha256=permit.venue_terms_sha256
                 AND observed.venue_execution_sha256=sha256(j.fill) AND fill.observation=j.fill
                 AND observed.next_state_sha256=permit.prior_state_sha256 AND observed.prior_version+1=permit.prior_version);
    IF NOT FOUND THEN RETURN 'HELD'; END IF;
    IF EXISTS(SELECT FROM public.synthetic_oms_observations o WHERE (o.tenant_id,o.order_id,o.operation)=(tenant,p_order,'cancel_pending')
              AND (o.writer_epoch,o.event_id,o.effective_seconds,o.effective_nanos,o.observation)
                  IS DISTINCT FROM (epoch,p_event,p_seconds,p_nanos,''::bytea)) THEN RETURN 'HELD'; END IF;
    SELECT EXISTS(SELECT FROM public.synthetic_oms_observations o WHERE (o.tenant_id,o.order_id,o.operation,o.event_id)=
        (tenant,p_order,'cancel_pending',p_event)) INTO claimed;
    IF NOT finish THEN
        IF claimed THEN
            IF (phase,status) IN (('cancel_pending',3),('unknown_cancel',3),('cancelled',5)) THEN RETURN 'QUERY'; END IF;
            RETURN 'HELD';
        END IF;
        IF (phase,status)<>('working',3) THEN RETURN 'HELD'; END IF;
        result:=public.synthetic_oms_apply_settlement(p_order,p_portfolio,p_event,'cancel_pending',p_seconds,p_nanos,''::bytea,NULL,NULL);
        IF result='APPLIED' THEN RETURN 'SEND'; END IF;
    ELSIF claimed AND body IS NOT NULL AND octet_length(body)<=65536
          AND octet_length(next_state) BETWEEN 1 AND 65536 AND octet_length(fact) BETWEEN 1 AND 65536 THEN
        IF (phase,status)=('cancelled',5) THEN
            -- The original kernel committed this observation, the authorized
            -- state/FACT and the capital adjustment in the same transaction.
            IF EXISTS(SELECT FROM public.synthetic_oms_observations ended
                JOIN public.synthetic_oms_settlement_authorizations receipt USING(tenant_id,order_id,event_id)
                WHERE (ended.tenant_id,ended.order_id,ended.event_id,ended.operation,ended.writer_epoch)=
                      (tenant,p_order,terminal_event,'cancelled',epoch)
                  AND (ended.effective_seconds,ended.effective_nanos,ended.observation)=(terminal_seconds,terminal_nanos,body)
                  AND (receipt.writer_epoch,receipt.operation,receipt.principal_id,receipt.portfolio_id,receipt.command_sha256)=
                      (epoch,'cancelled',p_principal,p_portfolio,command_sha)
                  AND receipt.prior_version+1=version AND receipt.next_state_sha256=state_sha
                  AND (receipt.body_sha256,receipt.next_state_sha256,receipt.fact_payload_sha256)=(sha256(body),sha256(next_state),sha256(fact))) THEN
                RETURN 'DUPLICATE';
            END IF;
        ELSIF status=3 AND phase IN ('cancel_pending','unknown_cancel') THEN
            result:=public.synthetic_oms_apply_settlement(p_order,p_portfolio,terminal_event,'cancelled',terminal_seconds,terminal_nanos,body,next_state,fact);
            IF result='APPLIED' THEN RETURN 'APPLIED'; END IF;
        END IF;
    END IF;
    RETURN 'HELD';
END $$;
ALTER FUNCTION public.synthetic_oms_go_cancel(boolean,text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_go_cancel(boolean,text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea) FROM PUBLIC;

CREATE FUNCTION public.synthetic_oms_go_cancel_claim(text,text,text,text,text,bigint,integer,bytea)
RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
    SELECT public.synthetic_oms_go_cancel(false,$1,$2,$3,$4,$5,$6,$7,$8,NULL,NULL,NULL,NULL,NULL,NULL)
$$;
ALTER FUNCTION public.synthetic_oms_go_cancel_claim(text,text,text,text,text,bigint,integer,bytea) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_go_cancel_claim(text,text,text,text,text,bigint,integer,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_go_cancel_claim(text,text,text,text,text,bigint,integer,bytea) TO kanz_synthetic_oms_go_working_runtime;

CREATE FUNCTION public.synthetic_oms_go_cancel_finish(text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea)
RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
    SELECT public.synthetic_oms_go_cancel(true,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
$$;
ALTER FUNCTION public.synthetic_oms_go_cancel_finish(text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_go_cancel_finish(text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_go_cancel_finish(text,text,text,text,text,bigint,integer,bytea,text,bigint,integer,bytea,bytea,bytea) TO kanz_synthetic_oms_go_working_runtime;
COMMIT;
