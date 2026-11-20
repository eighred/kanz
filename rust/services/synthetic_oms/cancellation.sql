-- Upgrade an admitted synthetic estate after 0028 and funding.sql.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database()<>'kanz_synthetic_oms' THEN
        RAISE EXCEPTION 'synthetic withdrawal requires its disposable database';
    END IF;
END $$;

-- Independent gateway evidence is per delivery: distinct signed events may
-- contain the same CancelOrder bytes. Only the control identity appends it.
CREATE TABLE public.synthetic_oms_cancellation_authorizations (
    tenant_id text NOT NULL DEFAULT public.app_current_tenant(),
    event_id text NOT NULL CHECK (octet_length(event_id) BETWEEN 1 AND 256),
    principal_id text NOT NULL,
    portfolio_id text NOT NULL,
    order_id text NOT NULL,
    command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256)=32),
    terminal_state_sha256 bytea CHECK (octet_length(terminal_state_sha256)=32),
    fact_payload_sha256 bytea CHECK (octet_length(fact_payload_sha256)=32),
    trace_context text NOT NULL DEFAULT '' CHECK (octet_length(trace_context)<=55),
    effective_at timestamptz NOT NULL CHECK (isfinite(effective_at)),
    observed_at timestamptz NOT NULL CHECK (isfinite(observed_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    PRIMARY KEY (tenant_id,event_id),
    CHECK (effective_at<=observed_at AND observed_at<expires_at)
);
CREATE TABLE public.synthetic_oms_funded_withdrawals (
    tenant_id text NOT NULL DEFAULT public.app_current_tenant(),
    order_id text NOT NULL,
    event_id text NOT NULL,
    principal_id text NOT NULL,
    portfolio_id text NOT NULL,
    command bytea NOT NULL CHECK (octet_length(command) BETWEEN 1 AND 65536),
    command_sha256 bytea GENERATED ALWAYS AS (sha256(command)) STORED,
    prior_state_sha256 bytea NOT NULL CHECK (octet_length(prior_state_sha256)=32),
    state bytea NOT NULL CHECK (octet_length(state) BETWEEN 1 AND 65536),
    fact_payload bytea NOT NULL CHECK (octet_length(fact_payload) BETWEEN 1 AND 65536),
    fact_row_id bigint,
    command_deadline timestamptz CHECK (isfinite(command_deadline)),
    order_version bigint NOT NULL DEFAULT 1 CHECK (order_version=1),
    writer_epoch bigint NOT NULL DEFAULT public.synthetic_oms_assert_writer(),
    PRIMARY KEY (tenant_id,order_id),
    UNIQUE (tenant_id,event_id)
);
ALTER TABLE public.synthetic_oms_cancellation_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.synthetic_oms_cancellation_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.synthetic_oms_cancellation_authorizations
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());
ALTER TABLE public.synthetic_oms_funded_withdrawals ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.synthetic_oms_funded_withdrawals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.synthetic_oms_funded_withdrawals
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());
GRANT INSERT ON public.synthetic_oms_cancellation_authorizations TO kanz_synthetic_oms_control;
GRANT SELECT ON public.synthetic_oms_cancellation_authorizations TO kanz_synthetic_oms_runtime,kanz_synthetic_funding_guard;
GRANT SELECT ON public.synthetic_oms_funded_withdrawals TO kanz_synthetic_oms_runtime;
GRANT SELECT (tenant_id,order_id,event_id,fact_row_id), UPDATE (fact_row_id)
    ON public.synthetic_oms_funded_withdrawals TO kanz_synthetic_funding_guard;
GRANT INSERT (order_id,event_id,principal_id,portfolio_id,command,prior_state_sha256,state,fact_payload,command_deadline)
    ON public.synthetic_oms_funded_withdrawals TO kanz_synthetic_oms_runtime;

GRANT SELECT (tenant_id,order_id,owner_order_id,currency,booked_debit,executed_debit,allocated_debit)
    ON public.capital_members TO kanz_synthetic_funding_guard;
GRANT SELECT (tenant_id,portfolio_id,order_id,currency,required_debit,admission_debit,booked_debit,executed_debit,allocated_debit,version),
      UPDATE (required_debit,version) ON public.capital_commitments TO kanz_synthetic_funding_guard;
GRANT EXECUTE ON FUNCTION public.capital_release_unexecuted(text,text,text[],text[]) TO kanz_synthetic_funding_guard;
GRANT SELECT (tenant_id,order_id,principal_id,portfolio_id,command_sha256,phase,evidence_ids), UPDATE (phase)
    ON public.synthetic_oms_commands TO kanz_synthetic_funding_guard;
GRANT SELECT (version,state,status,parent_order_id,allocated_quantity), UPDATE (state,status,version,updated_at)
    ON public.orders TO kanz_synthetic_funding_guard;
GRANT SELECT (tenant_id,order_id) ON public.synthetic_oms_observations,public.order_fills,public.order_executions
    TO kanz_synthetic_funding_guard;
GRANT SELECT (id,trace_context,event_time,event_class,schema_version,domain,payload) ON public.outbox TO kanz_synthetic_funding_guard;

CREATE FUNCTION public.synthetic_oms_withdraw_funded() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
    approval public.synthetic_oms_funding_authorizations%ROWTYPE;
    cancel_deadline timestamptz;
    outcome text;
BEGIN
    IF public.synthetic_oms_assert_writer() IS DISTINCT FROM NEW.writer_epoch
        OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
        OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER') THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='unowned withdrawal';
    END IF;
    SELECT least(a.expires_at,NEW.command_deadline) INTO cancel_deadline
      FROM public.synthetic_oms_cancellation_authorizations a
      WHERE (a.tenant_id,a.event_id,a.principal_id,a.portfolio_id,a.order_id,a.command_sha256)=
            (NEW.tenant_id,NEW.event_id,NEW.principal_id,NEW.portfolio_id,NEW.order_id,NEW.command_sha256)
        AND a.terminal_state_sha256=sha256(NEW.state) AND a.fact_payload_sha256=sha256(NEW.fact_payload)
        AND a.observed_at<=clock_timestamp();
    IF NOT FOUND OR cancel_deadline<=clock_timestamp() THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_unauthorized', MESSAGE='withdrawal evidence missing or expired';
    END IF;

    -- Read without locking the aggregate. The adapter hold excludes dispatch,
    -- and the receipt serializes competing withdrawals before the cash locks.
    SELECT a.* INTO approval FROM public.synthetic_oms_funding_authorizations a
      JOIN public.synthetic_oms_commands c USING (tenant_id,order_id,principal_id,portfolio_id,command_sha256)
      JOIN public.orders o USING (tenant_id,order_id,portfolio_id)
      WHERE (a.tenant_id,a.order_id,a.principal_id,a.portfolio_id)=
            (NEW.tenant_id,NEW.order_id,NEW.principal_id,NEW.portfolio_id)
        AND c.phase='accepted' AND o.status=1 AND o.version=0 AND o.parent_order_id=''
        AND o.allocated_quantity=0 AND sha256(o.state)=NEW.prior_state_sha256;
    IF NOT FOUND OR EXISTS (SELECT FROM public.synthetic_oms_observations WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
       OR EXISTS (SELECT FROM public.order_fills WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id))
       OR EXISTS (SELECT FROM public.order_executions WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.order_id)) THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_invalid_state', MESSAGE='withdrawal requires a direct never-dispatched funded order';
    END IF;
    outcome := public.capital_release_unexecuted(NEW.portfolio_id,NEW.order_id,approval.currencies,approval.amounts);
    IF outcome IS DISTINCT FROM 'RELEASED' THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_funding_unknown', MESSAGE='no pristine complete reservation';
    END IF;
    UPDATE public.orders SET status=5,state=NEW.state,version=version+1,updated_at=clock_timestamp()
        WHERE (tenant_id,order_id,portfolio_id)=(NEW.tenant_id,NEW.order_id,NEW.portfolio_id)
          AND status=1 AND version=0 AND parent_order_id='' AND allocated_quantity=0 AND sha256(state)=NEW.prior_state_sha256;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_invalid_state', MESSAGE='withdrawal lost order comparison';
    END IF;
    UPDATE public.synthetic_oms_commands SET phase='cancelled'
        WHERE (tenant_id,order_id,principal_id,portfolio_id)=(NEW.tenant_id,NEW.order_id,NEW.principal_id,NEW.portfolio_id)
          AND phase='accepted' AND command_sha256=approval.command_sha256;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_invalid_state', MESSAGE='withdrawal lost command comparison';
    END IF;
    IF cancel_deadline<=clock_timestamp() THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_unauthorized', MESSAGE='withdrawal evidence expired during lock acquisition';
    END IF;
    RETURN NULL;
END $$;
ALTER FUNCTION public.synthetic_oms_withdraw_funded() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_withdraw_funded() FROM PUBLIC;
CREATE TRIGGER withdraw_synthetic_funded AFTER INSERT ON public.synthetic_oms_funded_withdrawals
    FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_withdraw_funded();

-- A terminal FACT on a funded order consumes the receipt's single outbox
-- slot. The deferred check below verifies every envelope field and payload.
-- A second writer can neither publish an orphan nor reuse a committed slot.
CREATE FUNCTION public.synthetic_oms_claim_withdrawal_fact() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF EXISTS (SELECT FROM public.capital_members WHERE (tenant_id,order_id)=(NEW.tenant_id,NEW.partition_key)) THEN
        UPDATE public.synthetic_oms_funded_withdrawals SET fact_row_id=NEW.id
            WHERE (tenant_id,order_id,event_id)=(NEW.tenant_id,NEW.partition_key,NEW.causation_id) AND fact_row_id IS NULL;
        IF NOT FOUND THEN
            RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
                CONSTRAINT='cancellation_fact_unclaimed', MESSAGE='no pending funded withdrawal for terminal FACT';
        END IF;
    END IF;
    RETURN NEW;
END $$;
ALTER FUNCTION public.synthetic_oms_claim_withdrawal_fact() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_claim_withdrawal_fact() FROM PUBLIC;
CREATE TRIGGER require_withdrawal_fact_slot BEFORE INSERT ON public.outbox FOR EACH ROW
    WHEN (NEW.subject='order.order.cancelled' OR NEW.event_type='order.order.cancelled' OR NEW.payload_schema_ref='order.v1.OrderCancelled:1')
    EXECUTE FUNCTION public.synthetic_oms_claim_withdrawal_fact();

CREATE FUNCTION public.synthetic_oms_require_withdrawal_fact() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp SET app.tenant_id='' AS $$
BEGIN
    PERFORM set_config('app.tenant_id',NEW.tenant_id,true);
    IF NOT EXISTS (
        SELECT FROM public.outbox f
        JOIN public.synthetic_oms_commands c ON (c.tenant_id,c.order_id)=(f.tenant_id,f.partition_key)
        JOIN public.synthetic_oms_evidence e ON (e.tenant_id,e.order_id,e.principal_id,e.portfolio_id,e.command_sha256)=
            (c.tenant_id,c.order_id,c.principal_id,c.portfolio_id,c.command_sha256)
        JOIN public.synthetic_oms_cancellation_authorizations a ON (a.tenant_id,a.event_id)=(f.tenant_id,f.causation_id)
        JOIN public.synthetic_oms_funded_withdrawals w ON (w.tenant_id,w.order_id,w.fact_row_id)=(f.tenant_id,f.partition_key,f.id)
        WHERE (f.tenant_id,f.envelope_tenant_id,f.partition_key,f.causation_id)=(NEW.tenant_id,NEW.tenant_id,NEW.order_id,NEW.event_id)
          AND f.producer_service='oms' AND f.subject='order.order.cancelled' AND f.event_type=f.subject
          AND f.event_class=1 AND f.schema_version=1 AND f.domain='order' AND f.payload_schema_ref='order.v1.OrderCancelled:1'
          AND e.kind='entitlement' AND e.source_id=ANY(c.evidence_ids) AND f.correlation_id=e.source_id
          AND f.trace_context=a.trace_context AND f.event_time=a.effective_at AND f.payload=NEW.fact_payload
    ) THEN
        RAISE EXCEPTION USING ERRCODE='23514', SCHEMA=TG_TABLE_SCHEMA, TABLE=TG_TABLE_NAME,
            CONSTRAINT='cancellation_fact_missing', MESSAGE='withdrawal FACT missing';
    END IF;
    RETURN NULL;
END $$;
ALTER FUNCTION public.synthetic_oms_require_withdrawal_fact() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_require_withdrawal_fact() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER require_withdrawal_fact AFTER INSERT ON public.synthetic_oms_funded_withdrawals
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_require_withdrawal_fact();
COMMIT;
