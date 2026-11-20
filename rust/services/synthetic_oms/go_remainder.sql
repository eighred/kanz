-- Install in the disposable funded working estate after go_cancel.sql.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' OR
       to_regprocedure('public.synthetic_oms_go_cancel_claim(text,text,text,text,text,bigint,integer,bytea)') IS NULL THEN
        RAISE EXCEPTION 'funded Go working profile required';
    END IF;
END $$;

GRANT SELECT(order_id,raw_fill_id) ON public.order_fills TO kanz_synthetic_settlement_guard;
GRANT SELECT(venue,venue_account_id,instrument_id,venue_execution_id) ON public.order_executions TO kanz_synthetic_settlement_guard;
GRANT SELECT(payload,causation_id,trace_context) ON public.outbox TO kanz_synthetic_settlement_guard;

CREATE FUNCTION public.synthetic_oms_go_fill_remainder(
    p_portfolio text,p_order text,p_event text,p_seconds bigint,p_nanos integer,
    p_prior_fill bytea,p_new_fill bytea,p_next_state bytea,p_fact bytea)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
    tenant text:=public.app_current_tenant(); epoch bigint; evidence record; head record;
    fill_key text; event_at timestamptz; observation_digest bytea;
    zero CONSTANT numeric[]:=ARRAY[0,1]::numeric[];
    approved numeric[]; debit numeric[]; booked numeric[]; reserved numeric[]; remaining numeric[];
    bnb_reserved text; leg integer;
BEGIN
    IF NOT pg_has_role(session_user,'kanz_synthetic_oms_go_working_runtime','USAGE')
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER')
       OR pg_has_role(session_user,'kanz_synthetic_oms_control','MEMBER')
       OR has_function_privilege(session_user,'public.synthetic_oms_apply_settlement(text,text,text,text,bigint,integer,bytea,bytea,bytea)','EXECUTE')
       OR current_setting('transaction_isolation')<>'read committed'
       OR p_event IS NULL OR p_event!~'^xsim-remainder/[0-9a-f]{64}$'
       OR p_seconds IS NULL OR p_seconds NOT BETWEEN 0 AND 253402300799
       OR p_nanos IS NULL OR p_nanos NOT BETWEEN 0 AND 999999999
       OR EXISTS(SELECT FROM unnest(ARRAY[p_prior_fill,p_new_fill,p_next_state,p_fact]) body
                 WHERE body IS NULL OR octet_length(body) NOT BETWEEN 1 AND 65536)
       OR p_prior_fill=p_new_fill THEN RETURN 'HELD'; END IF;

    epoch:=public.synthetic_oms_assert_writer();
    PERFORM pg_advisory_xact_lock(hashtextextended(json_build_array(tenant,p_order)::text,1532));
    event_at:=to_timestamp(p_seconds)+(p_nanos/1000)*interval '1 microsecond';
    -- The venue verifier attests a complete two-fill history and the exact
    -- terminal aggregate and FACT. Bind the previously settled G2 execution
    -- to the predecessor of that transition, including after a lost reply.
    SELECT c.phase,o.status,o.version,o.state,f.amounts,a.executed_debits,a.prior_version,
           a.prior_state_sha256,a.observed_at,a.expires_at,e.source_id,e.trace_context INTO evidence
      FROM public.synthetic_oms_commands c
      JOIN public.orders o USING(tenant_id,order_id,portfolio_id)
      JOIN public.synthetic_oms_funding_authorizations f USING(tenant_id,order_id,portfolio_id,principal_id,command_sha256)
      JOIN public.synthetic_oms_settlement_authorizations a USING(tenant_id,order_id,portfolio_id,principal_id,command_sha256)
      JOIN public.synthetic_oms_evidence e USING(tenant_id,order_id,portfolio_id,principal_id,command_sha256)
      JOIN public.synthetic_oms_settlement_authorizations old ON
        (old.tenant_id,old.order_id,old.portfolio_id,old.principal_id,old.command_sha256)=
        (a.tenant_id,a.order_id,a.portfolio_id,a.principal_id,a.command_sha256)
      JOIN public.synthetic_oms_observations observed ON
        (observed.tenant_id,observed.order_id,observed.event_id,observed.operation,observed.writer_epoch)=
        (old.tenant_id,old.order_id,old.event_id,old.operation,old.writer_epoch)
      JOIN public.order_executions first_fill ON
        (first_fill.tenant_id,first_fill.order_id,first_fill.fill_id)=(old.tenant_id,old.order_id,old.event_id)
     WHERE (c.tenant_id,c.portfolio_id,c.order_id)=(tenant,p_portfolio,p_order)
       AND c.writer_epoch=epoch-2 AND c.go_gateway_frame IS NOT NULL AND c.go_funding_amounts=f.amounts
       AND e.kind='entitlement' AND e.decision='ALLOW' AND e.source_id=ANY(c.evidence_ids)
       AND o.parent_order_id='' AND o.allocated_quantity=0
       AND (f.venue,f.venue_account_id)=('XSIM','synthetic-spot') AND f.currencies=ARRAY['BNB','USD']
       AND (a.event_id,a.writer_epoch,a.operation,a.effective_seconds,a.effective_nanos)=(p_event,epoch,'fill',p_seconds,p_nanos)
       AND (a.body_sha256,a.venue_execution_sha256,a.next_state_sha256,a.fact_payload_sha256)=
           (sha256(p_new_fill),sha256(p_new_fill),sha256(p_next_state),sha256(p_fact))
       AND a.currencies=f.currencies AND array_dims(a.executed_debits)='[1:2]' AND array_dims(f.amounts)='[1:2]'
       AND old.operation='fill' AND old.writer_epoch=epoch-1 AND old.event_id~'^xsim-partial/[0-9a-f]{64}$'
       AND old.currencies=a.currencies AND old.executed_debits=a.executed_debits AND old.venue_terms_sha256=a.venue_terms_sha256
       AND old.prior_version+1=a.prior_version AND old.next_state_sha256=a.prior_state_sha256
       AND old.body_sha256=sha256(p_prior_fill) AND old.venue_execution_sha256=sha256(p_prior_fill)
       AND (observed.effective_seconds,observed.effective_nanos,observed.observation)=(old.effective_seconds,old.effective_nanos,p_prior_fill)
       AND (observed.effective_seconds,observed.effective_nanos)<(p_seconds,p_nanos)
       AND (first_fill.venue,first_fill.venue_account_id,first_fill.instrument_id,first_fill.venue_execution_id,first_fill.fill)=
           ('XSIM','synthetic-spot','SYNTH-USD',old.event_id,p_prior_fill)
       AND EXISTS(SELECT FROM public.order_fills x WHERE (x.tenant_id,x.order_id,x.raw_fill_id)=(tenant,p_order,old.event_id))
       AND EXISTS(SELECT FROM public.outbox x WHERE (x.tenant_id,x.producer_service,x.partition_key,x.subject,x.correlation_id,x.causation_id,x.trace_context)=
                   (tenant,'oms',p_order,'order.order.partially_filled',e.source_id,old.event_id,e.trace_context) AND sha256(x.payload)=old.fact_payload_sha256);
    IF NOT FOUND OR event_at>evidence.observed_at OR evidence.observed_at>clock_timestamp() THEN RETURN 'HELD'; END IF;

    fill_key:='execution:v1:'||encode(sha256(convert_to('["XSIM","synthetic-spot","SYNTH-USD",'||to_json(p_event)::text||']','UTF8')),'hex');
    IF (evidence.phase,evidence.status)=('filled',4) THEN
        IF evidence.version=evidence.prior_version+1 AND evidence.state=p_next_state AND EXISTS (
            SELECT FROM public.synthetic_oms_observations obs
            JOIN public.order_executions j ON (j.tenant_id,j.order_id,j.fill_id)=(obs.tenant_id,obs.order_id,obs.event_id)
            JOIN public.order_fills claim ON (claim.tenant_id,claim.order_id,claim.raw_fill_id)=(j.tenant_id,j.order_id,j.fill_id)
            WHERE (obs.tenant_id,obs.order_id,obs.event_id,obs.writer_epoch,obs.operation)=(tenant,p_order,p_event,epoch,'fill')
              AND (obs.effective_seconds,obs.effective_nanos,obs.observation)=(p_seconds,p_nanos,p_new_fill)
              AND claim.fill_id=fill_key AND (j.venue,j.venue_account_id,j.instrument_id,j.venue_execution_id,j.fill)=
                  ('XSIM','synthetic-spot','SYNTH-USD',p_event,p_new_fill))
           AND (SELECT count(*) FROM public.order_executions WHERE (tenant_id,order_id)=(tenant,p_order))=2
           AND (SELECT count(*) FROM public.order_fills WHERE (tenant_id,order_id)=(tenant,p_order))=2
           AND EXISTS(SELECT FROM public.outbox x WHERE (x.tenant_id,x.producer_service,x.partition_key,x.subject,x.correlation_id,x.causation_id,x.trace_context,x.payload)=
                       (tenant,'oms',p_order,'order.order.filled',evidence.source_id,p_event,evidence.trace_context,p_fact)) THEN RETURN 'DUPLICATE'; END IF;
        RETURN 'HELD';
    END IF;
    IF (evidence.phase,evidence.status,evidence.version) IS DISTINCT FROM ('working',3,evidence.prior_version)
       OR sha256(evidence.state)<>evidence.prior_state_sha256 OR evidence.expires_at<=clock_timestamp()
       OR (SELECT count(*) FROM public.order_executions WHERE (tenant_id,order_id)=(tenant,p_order))<>1
       OR (SELECT count(*) FROM public.order_fills WHERE (tenant_id,order_id)=(tenant,p_order))<>1
       OR EXISTS(SELECT FROM public.synthetic_oms_observations WHERE (tenant_id,order_id)=(tenant,p_order)
                 AND (writer_epoch IS DISTINCT FROM epoch-1 OR operation NOT IN ('dispatch','ack','timeout','fill')))
       OR NOT EXISTS(SELECT FROM public.capital_activation WHERE tenant_id=tenant AND enabled)
       OR EXISTS(SELECT FROM public.outbox WHERE published_at IS NULL AND producer_service IS NULL) THEN RETURN 'HELD'; END IF;

    -- Match the capital owner's lock order. Booked may be zero or the exact
    -- first debit. Only Accounting changes it or the total cash balance.
    PERFORM 1 FROM public.capital_source_health WHERE tenant_id=tenant AND fault_digest IS NULL FOR SHARE;
    IF NOT FOUND THEN RETURN 'HELD'; END IF;
    PERFORM b.currency FROM public.capital_balances b WHERE (b.tenant_id,b.portfolio_id)=(tenant,p_portfolio)
      AND b.currency IN ('BNB','USD') ORDER BY b.currency COLLATE "C" FOR UPDATE;
    IF EXISTS(SELECT FROM public.capital_members WHERE tenant_id=tenant AND (order_id=p_order OR owner_order_id=p_order)
              AND (owner_order_id<>p_order OR order_id<>p_order OR currency NOT IN ('BNB','USD')))
       OR EXISTS(SELECT FROM public.capital_commitments WHERE (tenant_id,order_id)=(tenant,p_order)
              AND (portfolio_id<>p_portfolio OR currency NOT IN ('BNB','USD'))) THEN RETURN 'HELD'; END IF;
    FOR leg IN 1..2 LOOP
        -- Accounting can attest this order's first booking in a partial
        -- portfolio report. The terminal fill consumes its existing hold;
        -- complete portfolio coverage is required at admission.
        SELECT b.reserved,b.total,c.version,c.admission_debit,c.required_debit,c.executed_debit,c.booked_debit,c.allocated_debit,
               m.executed_debit AS member_executed,m.booked_debit AS member_booked,m.allocated_debit AS member_allocated INTO head
         FROM public.capital_members m JOIN public.capital_commitments c USING(tenant_id,order_id,currency)
         JOIN public.capital_balances b USING(tenant_id,portfolio_id,currency)
         WHERE (c.tenant_id,c.portfolio_id,c.order_id)=(tenant,p_portfolio,p_order) AND m.owner_order_id=p_order
           AND c.currency=CASE leg WHEN 1 THEN 'BNB' ELSE 'USD' END
           AND b.revision>0 AND NOT b.conflicted AND b.gap_revision<=b.revision
         FOR UPDATE OF c,m;
        IF NOT FOUND THEN RETURN 'HELD'; END IF;
        approved:=public.capital_exact_parts(evidence.amounts[leg]); debit:=public.capital_exact_parts(evidence.executed_debits[leg]);
        reserved:=public.capital_exact_parts(head.reserved); booked:=public.capital_exact_parts(head.booked_debit);
        IF approved IS NULL OR approved[1]<=0 OR debit IS NULL OR booked IS NULL OR reserved IS NULL
           OR public.capital_exact_parts(head.total) IS NULL OR booked NOT IN (zero,debit)
           OR public.capital_exact_parts(head.admission_debit) IS DISTINCT FROM approved
           OR public.capital_exact_parts(head.required_debit) IS DISTINCT FROM approved
           OR public.capital_exact_parts(head.executed_debit) IS DISTINCT FROM debit
           OR public.capital_exact_parts(head.member_executed) IS DISTINCT FROM debit
           OR public.capital_exact_parts(head.member_booked) IS DISTINCT FROM booked
           OR public.capital_exact_parts(head.allocated_debit) IS DISTINCT FROM zero
           OR public.capital_exact_parts(head.member_allocated) IS DISTINCT FROM zero
           OR reserved[1]*approved[2]*booked[2]<reserved[2]*(approved[1]*booked[2]-booked[1]*approved[2]) THEN RETURN 'HELD'; END IF;
        IF leg=1 THEN
            IF head.version<>1 OR debit<>zero THEN RETURN 'HELD'; END IF;
            remaining:=public.capital_reduce_exact(reserved[1]*approved[2]-approved[1]*reserved[2],reserved[2]*approved[2]);
            IF remaining IS NULL THEN RETURN 'HELD'; END IF;
            bnb_reserved:=remaining[1]::text||CASE WHEN remaining[2]=1 THEN '' ELSE '/'||remaining[2]::text END;
        ELSIF head.version<>2 OR debit[1]<=0 OR public.capital_reduce_exact(2*debit[1],debit[2]) IS DISTINCT FROM approved THEN
            RETURN 'HELD';
        END IF;
    END LOOP;

    -- The symmetric USD terminal debit equals the original approval: its
    -- remaining reserve is unchanged until Accounting covers each fill. BNB
    -- had no execution, so FinishExecution releases that order's whole BNB hold.
    INSERT INTO public.order_fills(order_id,fill_id,raw_fill_id) VALUES(p_order,fill_key,p_event);
    INSERT INTO public.order_executions(order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill)
        VALUES(p_order,p_event,'XSIM','synthetic-spot','SYNTH-USD',p_event,p_new_fill);
    UPDATE public.capital_members SET executed_debit=evidence.amounts[2] WHERE (tenant_id,order_id,currency)=(tenant,p_order,'USD');
    UPDATE public.capital_commitments SET executed_debit=admission_debit,required_debit=admission_debit,version=version+2
        WHERE (tenant_id,portfolio_id,order_id,currency)=(tenant,p_portfolio,p_order,'USD');
    UPDATE public.capital_commitments SET required_debit='0',version=version+1 WHERE (tenant_id,portfolio_id,order_id,currency)=(tenant,p_portfolio,p_order,'BNB');
    UPDATE public.capital_balances SET reserved=bnb_reserved WHERE (tenant_id,portfolio_id,currency)=(tenant,p_portfolio,'BNB');

    UPDATE public.orders SET state=p_next_state,status=4,version=version+1,updated_at=clock_timestamp()
        WHERE (tenant_id,order_id,portfolio_id,version,status,state)=(tenant,p_order,p_portfolio,evidence.prior_version,3,evidence.state);
    IF NOT FOUND THEN RAISE EXCEPTION 'lost remainder aggregate comparison'; END IF;
    UPDATE public.synthetic_oms_commands SET phase='filled' WHERE (tenant_id,order_id,portfolio_id,phase)=(tenant,p_order,p_portfolio,'working');
    IF NOT FOUND THEN RAISE EXCEPTION 'lost remainder command comparison'; END IF;
    INSERT INTO public.outbox(producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,
        domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
    VALUES('oms',tenant,p_order,'order.order.filled','order.order.filled',1,1,'order','order.v1.OrderFilled:1',event_at,
           evidence.source_id,p_event,evidence.trace_context,p_fact);
    SELECT sha256(string_agg(int8send(octet_length(b)::bigint)||b,''::bytea ORDER BY n)) INTO observation_digest
        FROM unnest(ARRAY[convert_to('fill','UTF8'),p_new_fill,int8send(p_seconds),int4send(p_nanos)]) WITH ORDINALITY AS bytes(b,n);
    INSERT INTO public.synthetic_oms_observations(order_id,event_id,operation,observation,digest,effective_seconds,effective_nanos)
        VALUES(p_order,p_event,'fill',p_new_fill,observation_digest,p_seconds,p_nanos);
    IF evidence.expires_at<=clock_timestamp() THEN RAISE EXCEPTION USING ERRCODE='P1534', MESSAGE='remainder permit expired'; END IF;
    RETURN 'APPLIED';
EXCEPTION WHEN unique_violation OR SQLSTATE 'P1534' THEN
    -- The subtransaction rolls back every mutation on a conflicting identity
    -- or expiry while waiting for a financial row lock.
    RETURN 'HELD';
END $$;
ALTER FUNCTION public.synthetic_oms_go_fill_remainder(text,text,text,bigint,integer,bytea,bytea,bytea,bytea) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_go_fill_remainder(text,text,text,bigint,integer,bytea,bytea,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_go_fill_remainder(text,text,text,bigint,integer,bytea,bytea,bytea,bytea) TO kanz_synthetic_oms_go_working_runtime;
COMMIT;
