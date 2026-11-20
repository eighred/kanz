-- Install only in the disposable OMS database after funding, cancellation and 0029.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' THEN RAISE EXCEPTION 'synthetic settlement requires its disposable database'; END IF;
END $$;
-- The independent verifier binds each step to the original funded command,
-- the signed or typed delivery, and the exact canonical aggregate and FACT.
CREATE TABLE public.synthetic_oms_settlement_authorizations (
    tenant_id text NOT NULL DEFAULT public.app_current_tenant(),
    principal_id text NOT NULL, portfolio_id text NOT NULL, order_id text NOT NULL,
    command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256)=32),
    writer_epoch bigint NOT NULL CHECK (writer_epoch>0),
    operation text NOT NULL CHECK (operation IN ('dispatch','ack','cancel_pending','timeout','fill','cancelled')),
    event_id text NOT NULL CHECK (octet_length(event_id) BETWEEN 1 AND 256),
    effective_seconds bigint NOT NULL, effective_nanos integer NOT NULL CHECK (effective_nanos BETWEEN 0 AND 999999999),
    body_sha256 bytea NOT NULL CHECK (octet_length(body_sha256)=32),
    prior_version bigint NOT NULL CHECK (prior_version>=0), prior_state_sha256 bytea NOT NULL CHECK (octet_length(prior_state_sha256)=32),
    next_state_sha256 bytea CHECK (octet_length(next_state_sha256)=32),
    fact_payload_sha256 bytea CHECK (octet_length(fact_payload_sha256)=32),
    venue_terms_sha256 bytea NOT NULL CHECK (octet_length(venue_terms_sha256)=32),
    venue_execution_sha256 bytea CHECK (octet_length(venue_execution_sha256)=32),
    -- QueryOrder carries no venue cancellation timestamp. Preserve when the
    -- independent verifier actually observed the typed confirmation.
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(observed_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND observed_at<expires_at),
    currencies text[], executed_debits text[],
    CHECK ((operation IN ('ack','fill','cancelled'))=(fact_payload_sha256 IS NOT NULL)),
    CHECK (operation='timeout' OR (next_state_sha256 IS NOT NULL)=(fact_payload_sha256 IS NOT NULL)),
    CHECK ((operation IN ('fill','cancelled'))=(venue_execution_sha256 IS NOT NULL)
       AND (venue_execution_sha256 IS NOT NULL)=(currencies IS NOT NULL)
       AND (venue_execution_sha256 IS NOT NULL)=(executed_debits IS NOT NULL)),
    PRIMARY KEY (tenant_id,order_id,event_id)
);
ALTER TABLE public.synthetic_oms_settlement_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.synthetic_oms_settlement_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.synthetic_oms_settlement_authorizations
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());
GRANT INSERT ON public.synthetic_oms_settlement_authorizations TO kanz_synthetic_oms_control;
CREATE ROLE kanz_synthetic_settlement_guard NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS;
GRANT SELECT ON public.synthetic_oms_settlement_authorizations,public.synthetic_oms_evidence,
    public.synthetic_oms_funding_authorizations,public.synthetic_oms_cancellation_authorizations TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,order_id,principal_id,portfolio_id,phase,command_sha256,evidence_ids),UPDATE(phase)
    ON public.synthetic_oms_commands TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,order_id,portfolio_id,parent_order_id,version,status,state,allocated_quantity),
    UPDATE(status,state,version,updated_at) ON public.orders TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,order_id,event_id,operation,effective_seconds,effective_nanos),INSERT(order_id,event_id,operation,observation,digest,effective_seconds,effective_nanos)
    ON public.synthetic_oms_observations TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,fill_id),INSERT(order_id,fill_id,raw_fill_id) ON public.order_fills TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,order_id,fill_id,fill),INSERT(order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill)
    ON public.order_executions TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,producer_service,published_at,partition_key,subject,correlation_id),INSERT(producer_service,envelope_tenant_id,partition_key,
    subject,event_type,event_class,schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
    ON public.outbox TO kanz_synthetic_settlement_guard;
GRANT USAGE ON SEQUENCE public.outbox_id_seq TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,fault_digest),UPDATE(fault_digest) ON public.capital_source_health TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,enabled) ON public.capital_activation TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,portfolio_id,currency,total,reserved,revision,gap_revision,conflicted,coverage_complete,observed_at_ns),
    UPDATE(reserved) ON public.capital_balances TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,portfolio_id,order_id,currency,required_debit,admission_debit,booked_debit,executed_debit,allocated_debit,version),
    UPDATE(required_debit,executed_debit,version) ON public.capital_commitments TO kanz_synthetic_settlement_guard;
GRANT SELECT(tenant_id,order_id,owner_order_id,currency,booked_debit,executed_debit,allocated_debit),
    UPDATE(executed_debit) ON public.capital_members TO kanz_synthetic_settlement_guard;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_assert_writer(),public.capital_settle_direct(text,text,text[],text[],text[],boolean)
    TO kanz_synthetic_settlement_guard;
-- The adapter still cannot write funded state directly. Only SQL executed by
-- this NOLOGIN definer can pass the original six mutation guards.
DO $$
DECLARE tab text; operation text;
BEGIN
    FOR tab,operation IN SELECT * FROM (VALUES ('synthetic_oms_commands','UPDATE OF phase'),
        ('synthetic_oms_observations','INSERT'),('orders','INSERT OR UPDATE'),('order_fills','INSERT'),
        ('order_executions','INSERT'),('outbox','INSERT')) AS mutations(tab,operation)
    LOOP
        EXECUTE format('CREATE OR REPLACE TRIGGER hold_synthetic_funded_lifecycle BEFORE %s ON public.%I FOR EACH ROW
           WHEN (current_user<>''kanz_synthetic_settlement_guard'' AND pg_has_role(session_user,''kanz_synthetic_oms_adapter'',''USAGE''))
           EXECUTE FUNCTION public.synthetic_oms_hold_funded_lifecycle()',operation,tab);
    END LOOP;
END $$;
CREATE OR REPLACE TRIGGER require_withdrawal_fact_slot BEFORE INSERT ON public.outbox FOR EACH ROW
    WHEN (current_user<>'kanz_synthetic_settlement_guard' AND
        (NEW.subject='order.order.cancelled' OR NEW.event_type='order.order.cancelled' OR NEW.payload_schema_ref='order.v1.OrderCancelled:1'))
    EXECUTE FUNCTION public.synthetic_oms_claim_withdrawal_fact();

-- Accounting consumes the published bytes. A raw writer cannot substitute a
-- different partition or envelope tenant to publish a funded execution. Only
-- the legacy adapter can emit these facts in a wholly unarmed synthetic tenant.
CREATE FUNCTION public.synthetic_oms_require_execution_fact_owner() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF NEW.envelope_tenant_id IS DISTINCT FROM NEW.tenant_id OR NEW.producer_service IS DISTINCT FROM 'oms'
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_adapter','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_runtime','MEMBER')
       OR EXISTS (SELECT FROM public.capital_activation WHERE tenant_id=NEW.tenant_id AND enabled)
       OR EXISTS (SELECT FROM public.synthetic_oms_funding_authorizations WHERE tenant_id=NEW.tenant_id)
       OR NOT EXISTS (SELECT FROM public.synthetic_oms_commands WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.partition_key)) THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA,TABLE=TG_TABLE_NAME,
            CONSTRAINT='settlement_fact_unapproved',MESSAGE='execution FACT requires its scoped owner';
    END IF;
    RETURN NEW;
END $$;
ALTER FUNCTION public.synthetic_oms_require_execution_fact_owner() OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_require_execution_fact_owner() FROM PUBLIC;
CREATE TRIGGER enforce_execution_fact_owner BEFORE INSERT ON public.outbox FOR EACH ROW
    WHEN (current_user<>'kanz_synthetic_settlement_guard' AND
       (NEW.subject IN ('order.order.routed','order.order.filled','order.order.partially_filled','order.order.execution_recovered')
        OR NEW.event_type IN ('order.order.routed','order.order.filled','order.order.partially_filled','order.order.execution_recovered')
        OR NEW.payload_schema_ref IN ('order.v1.OrderRouted:1','order.v1.OrderFilled:1','order.v1.OrderPartiallyFilled:1','order.v1.ExecutionRecovered:1')))
    EXECUTE FUNCTION public.synthetic_oms_require_execution_fact_owner();

CREATE FUNCTION public.synthetic_oms_settlement_scope(p_order text,p_portfolio text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE tenant text:=public.app_current_tenant();
BEGIN
    IF NOT pg_has_role(session_user,'kanz_synthetic_oms_adapter','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_runtime','MEMBER') THEN RAISE EXCEPTION 'adapter identity required'; END IF;
    PERFORM public.synthetic_oms_assert_writer();
    IF NOT EXISTS(SELECT FROM public.synthetic_oms_commands c WHERE (c.tenant_id,c.order_id,c.portfolio_id)=(tenant,p_order,p_portfolio)
       AND (EXISTS(SELECT FROM public.capital_members m WHERE (m.tenant_id,m.order_id)=(c.tenant_id,c.order_id))
         OR EXISTS(SELECT FROM public.synthetic_oms_funding_authorizations f WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id))))
       THEN RETURN false; END IF;
    -- Serialize observation/replay across adapter generations before taking
    -- any cash or aggregate locks. Cash and orders retain their canonical CAS.
    PERFORM pg_advisory_xact_lock(hashtextextended(json_build_array(tenant,p_order)::text,1532));
    RETURN true;
END $$;
ALTER FUNCTION public.synthetic_oms_settlement_scope(text,text) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_settlement_scope(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_settlement_scope(text,text) TO kanz_synthetic_oms_adapter;

CREATE FUNCTION public.synthetic_oms_apply_settlement(p_order text,p_portfolio text,p_event text,p_op text,
    p_seconds bigint,p_nanos integer,p_body bytea,p_state bytea,p_fact bytea) RETURNS text LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
    tenant text:=public.app_current_tenant(); epoch bigint; checked_at timestamptz; event_at timestamptz; deadline timestamptz;
    auth public.synthetic_oms_settlement_authorizations%ROWTYPE;
    funding public.synthetic_oms_funding_authorizations%ROWTYPE;
    prior_phase text; next_phase text; old_status integer; next_status integer;
    root_event text; cause text:=p_event; trace text; fact_name text; subject text; key text; digest bytea; original_sources text[]; live_sources text[];
    leg integer; reserved numeric[]; approved numeric[]; total numeric[]; zero CONSTANT numeric[]:=ARRAY[0,1]::numeric[];
    observed_ns numeric; oldest_cash_ns numeric; max_age_ns CONSTANT numeric:=60000000000;
BEGIN
    IF NOT public.synthetic_oms_settlement_scope(p_order,p_portfolio) THEN RETURN 'HELD'; END IF;
    epoch:=public.synthetic_oms_assert_writer(); checked_at:=clock_timestamp();
    SELECT * INTO auth FROM public.synthetic_oms_settlement_authorizations
        WHERE (tenant_id,order_id,portfolio_id,event_id,writer_epoch,operation,effective_seconds,effective_nanos)=
              (tenant,p_order,p_portfolio,p_event,epoch,p_op,p_seconds,p_nanos);
    IF NOT FOUND OR auth.expires_at<=checked_at OR p_body IS NULL OR auth.body_sha256<>sha256(p_body)
       OR auth.next_state_sha256 IS DISTINCT FROM sha256(p_state) OR auth.fact_payload_sha256 IS DISTINCT FROM sha256(p_fact)
       OR octet_length(p_state)>65536 OR octet_length(p_fact)>65536 THEN RETURN 'HELD'; END IF;
    event_at:=to_timestamp(p_seconds)+(p_nanos/1000)*interval '1 microsecond';
    IF event_at>auth.observed_at OR auth.observed_at>checked_at THEN RETURN 'HELD'; END IF;
    deadline:=auth.expires_at;
    SELECT a.* INTO funding FROM public.synthetic_oms_funding_authorizations a
        WHERE (a.tenant_id,a.order_id,a.principal_id,a.portfolio_id,a.command_sha256)=
              (tenant,p_order,auth.principal_id,p_portfolio,auth.command_sha256);
    IF NOT FOUND THEN RETURN 'HELD'; END IF;
    SELECT c.phase,c.evidence_ids,o.status,e.source_id,e.trace_context INTO prior_phase,original_sources,old_status,root_event,trace
      FROM public.synthetic_oms_commands c JOIN public.orders o USING(tenant_id,order_id,portfolio_id)
      JOIN public.synthetic_oms_evidence e ON (e.tenant_id,e.order_id,e.principal_id,e.portfolio_id,e.command_sha256)=
            (c.tenant_id,c.order_id,c.principal_id,c.portfolio_id,c.command_sha256)
      WHERE (c.tenant_id,c.order_id,c.principal_id,c.portfolio_id,c.command_sha256)=
            (tenant,p_order,auth.principal_id,p_portfolio,auth.command_sha256)
        AND o.version=auth.prior_version AND sha256(o.state)=auth.prior_state_sha256 AND o.parent_order_id='' AND o.allocated_quantity=0
        AND e.kind='entitlement' AND e.source_id=ANY(c.evidence_ids);
    IF NOT FOUND OR EXISTS (SELECT FROM public.synthetic_oms_observations WHERE (tenant_id,order_id,event_id)=(tenant,p_order,p_event))
       THEN RETURN 'STATE'; END IF;
    IF p_op<>'dispatch' AND NOT EXISTS (
        SELECT FROM public.synthetic_oms_observations o JOIN public.synthetic_oms_settlement_authorizations d USING(tenant_id,order_id,event_id)
        WHERE (o.tenant_id,o.order_id,o.operation)=(tenant,p_order,'dispatch')
          AND d.command_sha256=auth.command_sha256 AND d.venue_terms_sha256=auth.venue_terms_sha256) THEN RETURN 'HELD'; END IF;
    next_status:=old_status;
    CASE p_op
      WHEN 'dispatch' THEN
        IF prior_phase<>'accepted' OR old_status<>1 OR auth.prior_version<>0 OR p_body<>''::bytea THEN RETURN 'STATE'; END IF;
        IF NOT EXISTS(SELECT FROM public.capital_activation WHERE tenant_id=tenant AND enabled) THEN RETURN 'HELD'; END IF;
        SELECT array_agg(source_id ORDER BY kind),least(deadline,funding.expires_at,min(expires_at),
            min(effective_at+interval '30 seconds') FILTER(WHERE kind='market')) INTO live_sources,deadline
          FROM public.synthetic_oms_evidence WHERE (tenant_id,order_id,principal_id,portfolio_id,command_sha256)=
            (tenant,p_order,auth.principal_id,p_portfolio,auth.command_sha256) AND instrument_id='SYNTH-USD' AND decision='ALLOW'
            AND effective_at<=observed_at AND observed_at<=checked_at;
        IF live_sources IS DISTINCT FROM original_sources OR cardinality(live_sources)<>5 OR deadline<=checked_at THEN RETURN 'HELD'; END IF;
        -- A retained approval alone cannot send the order. Hold the same
        -- source and currency locks as the ledger while proving every leg.
        PERFORM tenant_id FROM public.capital_source_health WHERE tenant_id=tenant AND fault_digest IS NULL FOR SHARE;
        IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
        IF (SELECT count(*) FROM public.capital_members WHERE tenant_id=tenant AND owner_order_id=p_order)<>cardinality(funding.currencies)
           OR (SELECT count(*) FROM public.capital_commitments WHERE tenant_id=tenant AND order_id=p_order)<>cardinality(funding.currencies)
           THEN RETURN 'UNKNOWN'; END IF;
        FOR leg IN 1..cardinality(funding.currencies) LOOP
            approved:=public.capital_exact_parts(funding.amounts[leg]);
            SELECT public.capital_exact_parts(b.reserved),public.capital_exact_parts(b.total),b.observed_at_ns INTO reserved,total,observed_ns
              FROM public.capital_balances b WHERE (b.tenant_id,b.portfolio_id,b.currency)=(tenant,p_portfolio,funding.currencies[leg])
                AND b.revision>0 AND b.coverage_complete AND NOT b.conflicted AND b.gap_revision<=b.revision FOR SHARE;
            IF NOT FOUND OR observed_ns IS NULL OR observed_ns>extract(epoch FROM clock_timestamp())*1000000000
               OR approved IS NULL OR reserved IS NULL OR total IS NULL OR total[1]*reserved[2]<reserved[1]*total[2]
               OR reserved[1]*approved[2]<approved[1]*reserved[2]
               OR NOT EXISTS(SELECT FROM public.capital_commitments c JOIN public.capital_members m USING(tenant_id,order_id,currency)
                  WHERE (c.tenant_id,c.portfolio_id,c.order_id,c.currency)=(tenant,p_portfolio,p_order,funding.currencies[leg])
                    AND c.version=1 AND m.owner_order_id=p_order AND public.capital_exact_parts(c.admission_debit)=approved
                    AND public.capital_exact_parts(c.required_debit)=approved AND public.capital_exact_parts(c.executed_debit)=zero
                    AND public.capital_exact_parts(c.booked_debit)=zero AND public.capital_exact_parts(c.allocated_debit)=zero
                    AND public.capital_exact_parts(m.executed_debit)=zero AND public.capital_exact_parts(m.booked_debit)=zero
                    AND public.capital_exact_parts(m.allocated_debit)=zero) THEN RETURN 'UNKNOWN'; END IF;
            oldest_cash_ns:=least(oldest_cash_ns,observed_ns);
        END LOOP;
        IF oldest_cash_ns+max_age_ns<extract(epoch FROM clock_timestamp())*1000000000 THEN RETURN 'UNKNOWN'; END IF;
        next_phase:='dispatch_claimed';
      WHEN 'ack' THEN
        IF prior_phase NOT IN ('dispatch_claimed','unknown_execute') OR old_status<>1 OR p_body<>convert_to(p_order,'UTF8') THEN RETURN 'STATE'; END IF;
        next_phase:='working'; next_status:=2; fact_name:='OrderRouted'; subject:='order.order.routed';
      WHEN 'cancel_pending' THEN
        IF prior_phase<>'working' OR old_status NOT IN (2,3) OR p_body<>''::bytea OR (old_status=3 AND NOT EXISTS (
            SELECT FROM public.synthetic_oms_observations o WHERE (o.tenant_id,o.order_id,o.operation)=(tenant,p_order,'fill')
                AND (o.effective_seconds,o.effective_nanos)<=(p_seconds,p_nanos))) THEN RETURN 'STATE'; END IF;
        SELECT least(deadline,expires_at) INTO deadline FROM public.synthetic_oms_cancellation_authorizations
          WHERE (tenant_id,order_id,principal_id,portfolio_id,event_id)=(tenant,p_order,auth.principal_id,p_portfolio,p_event)
            AND effective_at=event_at AND observed_at<=checked_at AND expires_at>checked_at;
        IF NOT FOUND THEN RETURN 'HELD'; END IF;
        next_phase:='cancel_pending';
      WHEN 'timeout' THEN
        IF prior_phase NOT IN ('dispatch_claimed','unknown_execute','cancel_pending','unknown_cancel') OR p_body<>''::bytea THEN RETURN 'STATE'; END IF;
        next_phase:=CASE WHEN prior_phase IN ('dispatch_claimed','unknown_execute') THEN 'unknown_execute' ELSE 'unknown_cancel' END;
      WHEN 'fill','cancelled' THEN
        IF auth.currencies IS DISTINCT FROM funding.currencies THEN RETURN 'STATE'; END IF;
        IF p_op='fill' THEN
            IF prior_phase NOT IN ('working','cancel_pending','unknown_cancel') OR old_status<>2 OR auth.venue_execution_sha256<>sha256(p_body)
               OR p_event!~'^xsim-partial/[0-9a-f]{64}$'
               OR EXISTS (SELECT FROM public.order_executions WHERE (tenant_id,order_id)=(tenant,p_order)) THEN RETURN 'STATE'; END IF;
            next_status:=3; next_phase:=prior_phase; fact_name:='OrderPartiallyFilled'; subject:='order.order.partially_filled';
        ELSE
            IF prior_phase NOT IN ('cancel_pending','unknown_cancel') OR old_status<>3 OR NOT EXISTS (
                SELECT FROM public.order_executions j JOIN public.synthetic_oms_settlement_authorizations a
                    ON (a.tenant_id,a.order_id,a.event_id)=(j.tenant_id,j.order_id,j.fill_id)
                WHERE (j.tenant_id,j.order_id)=(tenant,p_order) AND a.operation='fill' AND a.executed_debits=auth.executed_debits
                  AND a.currencies=auth.currencies AND sha256(j.fill)=auth.venue_execution_sha256 AND p_event=j.fill_id||'/cancelled'
            ) THEN RETURN 'STATE'; END IF;
            SELECT o.event_id,a.trace_context INTO cause,trace FROM public.synthetic_oms_observations o
              JOIN public.synthetic_oms_cancellation_authorizations a USING(tenant_id,order_id,event_id)
              JOIN public.synthetic_oms_observations f ON (f.tenant_id,f.order_id,f.operation)=(o.tenant_id,o.order_id,'fill')
              WHERE (o.tenant_id,o.order_id,o.operation)=(tenant,p_order,'cancel_pending')
                AND p_event=f.event_id||'/cancelled'
                AND greatest((o.effective_seconds,o.effective_nanos),(f.effective_seconds,f.effective_nanos))=(p_seconds,p_nanos)
                AND (a.principal_id,a.portfolio_id)=(auth.principal_id,p_portfolio);
            IF NOT FOUND THEN RETURN 'HELD'; END IF;
            -- Effective command time and actual knowledge time are distinct.
            -- Only the latter is evidence that XSIM confirmed the withdrawal.
            event_at:=auth.observed_at;
            next_status:=5; next_phase:='cancelled'; fact_name:='OrderCancelled'; subject:='order.order.cancelled';
        END IF;
        IF public.capital_settle_direct(p_portfolio,p_order,funding.currencies,funding.amounts,auth.executed_debits,p_op='cancelled')
             IS DISTINCT FROM 'APPLIED' THEN RETURN 'UNKNOWN'; END IF;
      ELSE RETURN 'HELD';
    END CASE;
    -- Any failure after the cash kernel must abort the whole transaction.
    IF p_state IS NOT NULL THEN
        UPDATE public.orders SET state=p_state,status=next_status,version=version+1,updated_at=clock_timestamp()
            WHERE (tenant_id,order_id,portfolio_id,version,status)=(tenant,p_order,p_portfolio,auth.prior_version,old_status)
              AND sha256(state)=auth.prior_state_sha256;
        IF NOT FOUND THEN RAISE EXCEPTION 'settlement lost order comparison'; END IF;
    END IF;
    UPDATE public.synthetic_oms_commands SET phase=next_phase WHERE (tenant_id,order_id,portfolio_id)=(tenant,p_order,p_portfolio)
        AND synthetic_oms_commands.phase=prior_phase AND command_sha256=auth.command_sha256;
    IF NOT FOUND THEN RAISE EXCEPTION 'settlement lost command comparison'; END IF;
    IF p_op='fill' THEN
        key:='execution:v1:'||encode(sha256(convert_to('["XSIM","synthetic-spot","SYNTH-USD",'||to_json(p_event)::text||']','UTF8')),'hex');
        INSERT INTO public.order_fills(order_id,fill_id,raw_fill_id) VALUES(p_order,key,p_event);
        INSERT INTO public.order_executions(order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill)
            VALUES(p_order,p_event,'XSIM','synthetic-spot','SYNTH-USD',p_event,p_body);
    END IF;
    IF p_fact IS NOT NULL THEN
        INSERT INTO public.outbox(producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,
            domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
            SELECT 'oms',tenant,p_order,subject,subject,1,1,'order','order.v1.'||fact_name||':1',event_at,root_event,cause,trace,p_fact
            WHERE NOT EXISTS (SELECT FROM public.outbox WHERE published_at IS NULL AND producer_service IS NULL);
        IF NOT FOUND THEN RAISE EXCEPTION 'unowned settlement outbox'; END IF;
    END IF;
    SELECT sha256(string_agg(int8send(octet_length(b)::bigint)||b,''::bytea ORDER BY n)) INTO digest
        FROM unnest(ARRAY[convert_to(p_op,'UTF8'),p_body,int8send(p_seconds),int4send(p_nanos)]) WITH ORDINALITY AS bytes(b,n);
    INSERT INTO public.synthetic_oms_observations(order_id,event_id,operation,observation,digest,effective_seconds,effective_nanos)
        VALUES(p_order,p_event,p_op,p_body,digest,p_seconds,p_nanos);
    IF deadline<=clock_timestamp() THEN
        RAISE EXCEPTION USING ERRCODE='23514',SCHEMA='public',TABLE='synthetic_oms_settlement_authorizations',
            CONSTRAINT='settlement_evidence_expired',MESSAGE='settlement evidence expired during lock acquisition';
    END IF;
    IF p_op='dispatch' AND oldest_cash_ns+max_age_ns<extract(epoch FROM clock_timestamp())*1000000000 THEN
        RAISE EXCEPTION USING ERRCODE='23514',SCHEMA='public',TABLE='synthetic_oms_settlement_authorizations',
            CONSTRAINT='settlement_cash_expired',MESSAGE='dispatch cash observation expired during lock acquisition';
    END IF;
    RETURN 'APPLIED';
END $$;
ALTER FUNCTION public.synthetic_oms_apply_settlement(text,text,text,text,bigint,integer,bytea,bytea,bytea) OWNER TO kanz_synthetic_settlement_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_apply_settlement(text,text,text,text,bigint,integer,bytea,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.synthetic_oms_apply_settlement(text,text,text,text,bigint,integer,bytea,bytea,bytea) TO kanz_synthetic_oms_adapter;
COMMIT;
