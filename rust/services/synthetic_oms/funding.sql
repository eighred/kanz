-- Install only in the disposable OMS estate after writer.sql and migration 0027.
BEGIN;
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $$ BEGIN
    IF current_database() <> 'kanz_synthetic_oms' THEN
        RAISE EXCEPTION 'synthetic funding requires its isolated database';
    END IF;
END $$;

CREATE TABLE public.synthetic_oms_funding_authorizations (
    tenant_id text NOT NULL DEFAULT public.app_current_tenant(),
    principal_id text NOT NULL,
    portfolio_id text NOT NULL,
    order_id text NOT NULL,
    command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256)=32),
    venue text NOT NULL CHECK (venue='XSIM'),
    venue_account_id text NOT NULL CHECK (venue_account_id='synthetic-spot'),
    currencies text[] NOT NULL,
    amounts text[] NOT NULL,
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    PRIMARY KEY (tenant_id,order_id),
    CHECK (cardinality(currencies) BETWEEN 1 AND 32 AND cardinality(amounts)=cardinality(currencies)
        AND array_ndims(currencies)=1 AND array_ndims(amounts)=1
        AND array_lower(currencies,1)=1 AND array_lower(amounts,1)=1
        AND array_position(currencies,NULL) IS NULL AND array_position(amounts,NULL) IS NULL)
);
ALTER TABLE public.synthetic_oms_funding_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.synthetic_oms_funding_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.synthetic_oms_funding_authorizations
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());
GRANT INSERT ON public.synthetic_oms_funding_authorizations TO kanz_synthetic_oms_funding;

-- The issuer can only append approvals. The trigger can reserve independently
-- approved amounts. Neither login performing the order lifecycle inherits it.
CREATE ROLE kanz_synthetic_funding_guard NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS;
GRANT SELECT ON public.synthetic_oms_funding_authorizations,public.synthetic_oms_evidence TO kanz_synthetic_funding_guard;
GRANT SELECT(tenant_id,enabled) ON public.capital_activation TO kanz_synthetic_funding_guard;
GRANT SELECT(tenant_id,fault_digest), UPDATE(fault_digest) ON public.capital_source_health TO kanz_synthetic_funding_guard;
GRANT SELECT(tenant_id,portfolio_id,currency,revision,gap_revision,conflicted,coverage_complete,total,reserved,observed_at_ns),
      UPDATE(reserved) ON public.capital_balances TO kanz_synthetic_funding_guard;
GRANT INSERT(order_id,portfolio_id,currency,required_debit,admission_debit)
    ON public.capital_commitments TO kanz_synthetic_funding_guard;
GRANT SELECT(tenant_id,order_id), INSERT(order_id,owner_order_id,currency)
    ON public.capital_members TO kanz_synthetic_funding_guard;
GRANT EXECUTE ON FUNCTION public.capital_reserve(text,text,text,text,bigint,integer,bigint),
    public.synthetic_oms_assert_writer() TO kanz_synthetic_funding_guard;

CREATE FUNCTION public.synthetic_oms_reserve_authorized() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
    approval public.synthetic_oms_funding_authorizations%ROWTYPE;
    sources text[];
    deadline timestamptz;
    checked_at timestamptz;
    epoch_seconds numeric;
    seconds bigint;
    nanos integer;
    max_age_ns CONSTANT bigint := 60000000000;
    i integer;
    outcome text;
BEGIN
    SELECT * INTO approval FROM public.synthetic_oms_funding_authorizations
        WHERE tenant_id=NEW.tenant_id AND order_id=NEW.order_id;
    IF NOT FOUND THEN
        -- An unfunded insertion still goes through the canonical activation
        -- trigger on orders. An orphan membership cannot authorize a new claim.
        IF EXISTS (SELECT FROM public.capital_members WHERE tenant_id=NEW.tenant_id AND order_id=NEW.order_id) THEN
            RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_authorization_invalid',
                TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding authorization missing';
        END IF;
        RETURN NULL;
    END IF;

    IF public.synthetic_oms_assert_writer() IS DISTINCT FROM NEW.writer_epoch
       OR NOT pg_has_role(session_user,'kanz_synthetic_oms_runtime','USAGE')
       OR pg_has_role(session_user,'kanz_synthetic_oms_adapter','MEMBER')
       OR NEW.phase <> 'accepted'
       OR (approval.principal_id,approval.portfolio_id,approval.command_sha256)
           IS DISTINCT FROM (NEW.principal_id,NEW.portfolio_id,NEW.command_sha256)
       OR NEW.command_sha256 <> sha256(NEW.command)
       OR NOT EXISTS (SELECT FROM public.capital_activation WHERE tenant_id=NEW.tenant_id AND enabled) THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_authorization_invalid',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding approval or activation refused';
    END IF;

    checked_at := clock_timestamp();
    SELECT array_agg(source_id ORDER BY kind),
           least(min(expires_at),min(effective_at + interval '30 seconds') FILTER (WHERE kind='market'),approval.expires_at)
      INTO sources,deadline FROM public.synthetic_oms_evidence
      WHERE (tenant_id,principal_id,portfolio_id,order_id,command_sha256)=
            (NEW.tenant_id,NEW.principal_id,NEW.portfolio_id,NEW.order_id,NEW.command_sha256)
        AND instrument_id='SYNTH-USD' AND decision='ALLOW'
        AND effective_at<=observed_at AND observed_at<=checked_at;
    IF sources IS DISTINCT FROM NEW.evidence_ids OR cardinality(sources) <> 5 OR deadline <= checked_at THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_authorization_invalid',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding control evidence refused';
    END IF;

    epoch_seconds := extract(epoch FROM checked_at);
    seconds := floor(epoch_seconds)::bigint;
    nanos := ((epoch_seconds-seconds)*1000000000)::integer;
    FOR i IN 1..cardinality(approval.currencies) LOOP
        IF i>1 AND approval.currencies[i-1] COLLATE "C" >= approval.currencies[i] COLLATE "C" THEN
            RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_authorization_invalid',
                TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding currencies must be unique and sorted';
        END IF;
        outcome := public.capital_reserve(NEW.portfolio_id,approval.currencies[i],NEW.order_id,
                                          approval.amounts[i],seconds,nanos,max_age_ns);
        IF outcome IS DISTINCT FROM 'RESERVED' THEN
            IF outcome IS NULL OR outcome NOT IN ('UNKNOWN','INSUFFICIENT','INVALID','CONFLICT') THEN
                RAISE EXCEPTION 'unexpected capital reservation result';
            END IF;
            RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_'||lower(outcome),
                TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='capital reservation refused';
        END IF;
    END LOOP;

    -- All currency rows are now locked through COMMIT. A later-currency wait
    -- must not admit an expired approval, control, or opening cash observation.
    checked_at := clock_timestamp();
    IF deadline <= checked_at THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_authorization_invalid',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding authorization expired';
    END IF;
    IF EXISTS (SELECT FROM public.capital_balances WHERE tenant_id=NEW.tenant_id AND portfolio_id=NEW.portfolio_id
               AND currency=ANY(approval.currencies)
               AND observed_at_ns + max_age_ns::numeric < extract(epoch FROM checked_at)*1000000000) THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funding_unknown',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funding cash observation expired';
    END IF;
    RETURN NULL;
END $$;
ALTER FUNCTION public.synthetic_oms_reserve_authorized() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_reserve_authorized() FROM PUBLIC;
CREATE TRIGGER reserve_synthetic_funding AFTER INSERT ON public.synthetic_oms_commands
    FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_reserve_authorized();

-- The claim precedes the order and FACT. Check the complete write set at
-- commit, so a partial caller cannot leave a reservation without its order.
GRANT SELECT(tenant_id,order_id,portfolio_id) ON public.orders TO kanz_synthetic_funding_guard;
GRANT SELECT(tenant_id,envelope_tenant_id,partition_key,producer_service,subject,event_type,
             payload_schema_ref,correlation_id,causation_id) ON public.outbox TO kanz_synthetic_funding_guard;
CREATE FUNCTION public.synthetic_oms_require_funded_receipt() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp SET app.tenant_id='' AS $$
BEGIN
    PERFORM set_config('app.tenant_id',NEW.tenant_id,true);
    IF EXISTS (SELECT FROM public.synthetic_oms_funding_authorizations a
               JOIN public.capital_members m USING (tenant_id,order_id)
               WHERE (a.tenant_id,a.order_id,a.command_sha256)=(NEW.tenant_id,NEW.order_id,NEW.command_sha256))
       AND NOT EXISTS (
           SELECT FROM public.orders o
           JOIN public.outbox f ON f.tenant_id=o.tenant_id AND f.partition_key=o.order_id
           JOIN public.synthetic_oms_evidence e ON e.tenant_id=o.tenant_id AND e.order_id=o.order_id
           WHERE (o.tenant_id,o.order_id,o.portfolio_id)=(NEW.tenant_id,NEW.order_id,NEW.portfolio_id)
             AND (e.principal_id,e.portfolio_id,e.command_sha256)=(NEW.principal_id,NEW.portfolio_id,NEW.command_sha256)
             AND e.kind='entitlement' AND f.envelope_tenant_id=NEW.tenant_id AND f.producer_service='oms'
             AND f.subject='order.order.accepted' AND f.event_type='order.order.accepted'
             AND f.payload_schema_ref='order.v1.OrderAccepted:1'
             AND f.correlation_id=e.source_id AND f.causation_id=e.source_id
       ) THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funded_receipt_incomplete',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funded order or accepted FACT missing';
    END IF;
    RETURN NULL;
END $$;
ALTER FUNCTION public.synthetic_oms_require_funded_receipt() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_require_funded_receipt() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER require_funded_receipt AFTER INSERT ON public.synthetic_oms_commands
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.synthetic_oms_require_funded_receipt();

-- A funded acceptance is observable, but the synthetic adapter cannot claim
-- dispatch or record a fill/cancellation until funded settlement is implemented.
CREATE FUNCTION public.synthetic_oms_hold_funded_lifecycle() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
    SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE id text;
BEGIN
    IF TG_TABLE_NAME='outbox' THEN
        id := NEW.partition_key;
    ELSE
        id := NEW.order_id;
    END IF;
    IF EXISTS (SELECT FROM public.capital_members WHERE tenant_id=NEW.tenant_id AND order_id=id) THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='funded_lifecycle_held',
            TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA, MESSAGE='funded settlement not enabled';
    END IF;
    RETURN NEW;
END $$;
ALTER FUNCTION public.synthetic_oms_hold_funded_lifecycle() OWNER TO kanz_synthetic_funding_guard;
REVOKE ALL ON FUNCTION public.synthetic_oms_hold_funded_lifecycle() FROM PUBLIC;
DO $$
DECLARE tab text; operation text;
BEGIN
    FOR tab,operation IN SELECT * FROM (VALUES ('synthetic_oms_commands','UPDATE OF phase'),
        ('synthetic_oms_observations','INSERT'),('orders','INSERT OR UPDATE'),('order_fills','INSERT'),
        ('order_executions','INSERT'),('outbox','INSERT')) AS mutations(tab,operation)
    LOOP
        EXECUTE format('CREATE TRIGGER hold_synthetic_funded_lifecycle BEFORE %s ON public.%I
            FOR EACH ROW WHEN (pg_has_role(session_user,''kanz_synthetic_oms_adapter'',''USAGE''))
            EXECUTE FUNCTION public.synthetic_oms_hold_funded_lifecycle()',operation,tab);
    END LOOP;
END $$;
COMMIT;
