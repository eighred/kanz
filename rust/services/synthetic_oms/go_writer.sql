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
CREATE ROLE kanz_synthetic_oms_go_funded_runtime NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS;
GRANT kanz_synthetic_oms_go_runtime TO kanz_synthetic_oms_go_funded_runtime;

-- The writer lock has drained the old adapter. An eligible funded order has
-- one observed partial fill and a confirmed cancellation. The cash ledger has
-- not booked its fill yet, so the execution debit must still be held.
CREATE FUNCTION public.synthetic_oms_check_funded_go_handback(old_epoch bigint) RETURNS void LANGUAGE plpgsql
    SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
BEGIN
    -- Accounting may book against the same hold without changing the writer
    -- epoch. Use its source-then-cash lock order and keep the rows locked until
    -- the switch commits. Under READ COMMITTED, validate after any wait.
    PERFORM h.tenant_id FROM public.capital_source_health h
    WHERE EXISTS(SELECT FROM public.synthetic_oms_commands c WHERE c.tenant_id=h.tenant_id)
    ORDER BY h.tenant_id COLLATE "C" FOR SHARE OF h;
    PERFORM b.currency FROM public.capital_balances b
    WHERE EXISTS(SELECT FROM public.synthetic_oms_commands c
                 WHERE (c.tenant_id,c.portfolio_id)=(b.tenant_id,b.portfolio_id))
    ORDER BY b.tenant_id COLLATE "C",b.portfolio_id COLLATE "C",b.currency COLLATE "C" FOR UPDATE OF b;

    IF EXISTS (
        SELECT FROM public.synthetic_oms_commands c
        JOIN public.orders o USING(tenant_id,order_id)
        WHERE c.phase<>'cancelled' OR o.status<>5 OR o.parent_order_id<>''
           OR o.portfolio_id IS DISTINCT FROM c.portfolio_id OR c.go_gateway_frame IS NULL
           OR NOT EXISTS (
              SELECT FROM public.synthetic_oms_funding_authorizations funding
              JOIN public.synthetic_oms_observations terminal USING(tenant_id,order_id)
              JOIN public.synthetic_oms_settlement_authorizations receipt USING(tenant_id,order_id,event_id)
              JOIN public.order_executions fill USING(tenant_id,order_id)
              JOIN public.synthetic_oms_settlement_authorizations execution
                ON (execution.tenant_id,execution.order_id,execution.event_id)=(fill.tenant_id,fill.order_id,fill.fill_id)
              JOIN public.synthetic_oms_observations executed
                ON (executed.tenant_id,executed.order_id,executed.event_id)=(fill.tenant_id,fill.order_id,fill.fill_id)
              JOIN public.synthetic_oms_observations cancel_claim
                ON (cancel_claim.tenant_id,cancel_claim.order_id)=(receipt.tenant_id,receipt.order_id)
              JOIN public.synthetic_oms_cancellation_authorizations cancel_source
                ON (cancel_source.tenant_id,cancel_source.order_id,cancel_source.event_id)=
                   (cancel_claim.tenant_id,cancel_claim.order_id,cancel_claim.event_id)
              WHERE (funding.tenant_id,funding.order_id,funding.portfolio_id,funding.principal_id,funding.command_sha256)=
                    (c.tenant_id,c.order_id,c.portfolio_id,c.principal_id,c.command_sha256)
                AND terminal.operation='cancelled' AND receipt.operation=terminal.operation
                AND terminal.writer_epoch=old_epoch AND receipt.writer_epoch=old_epoch
                AND (receipt.portfolio_id,receipt.principal_id,receipt.command_sha256)=(c.portfolio_id,c.principal_id,c.command_sha256)
                AND EXISTS (SELECT FROM public.capital_source_health h WHERE h.tenant_id=c.tenant_id AND h.fault_digest IS NULL)
                AND receipt.prior_version+1=o.version AND receipt.next_state_sha256=sha256(o.state)
                AND (receipt.effective_seconds,receipt.effective_nanos)=(terminal.effective_seconds,terminal.effective_nanos)
                AND receipt.body_sha256=sha256(terminal.observation)
                AND terminal.event_id=fill.fill_id||'/cancelled' AND execution.operation='fill' AND executed.operation='fill'
                AND execution.writer_epoch=executed.writer_epoch AND execution.body_sha256=sha256(executed.observation)
                AND (execution.effective_seconds,execution.effective_nanos)=(executed.effective_seconds,executed.effective_nanos)
                AND (execution.portfolio_id,execution.principal_id,execution.command_sha256)=(c.portfolio_id,c.principal_id,c.command_sha256)
                AND execution.next_state_sha256=receipt.prior_state_sha256 AND execution.prior_version+1=receipt.prior_version
                AND receipt.venue_execution_sha256=sha256(fill.fill)
                AND cancel_claim.operation='cancel_pending'
                AND (cancel_source.portfolio_id,cancel_source.principal_id)=(c.portfolio_id,c.principal_id)
                AND (terminal.effective_seconds,terminal.effective_nanos)=
                    greatest((cancel_claim.effective_seconds,cancel_claim.effective_nanos),(executed.effective_seconds,executed.effective_nanos))
                AND (execution.venue_execution_sha256,execution.currencies,execution.executed_debits)=
                    (receipt.venue_execution_sha256,receipt.currencies,receipt.executed_debits)
                AND receipt.currencies=funding.currencies
                AND (SELECT count(*) FROM public.order_executions e WHERE (e.tenant_id,e.order_id)=(c.tenant_id,c.order_id))=1
                AND (SELECT count(*) FROM public.order_fills f WHERE (f.tenant_id,f.order_id)=(c.tenant_id,c.order_id)
                     AND f.raw_fill_id=fill.fill_id)=1
                AND (SELECT count(*) FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(c.tenant_id,c.order_id))=4
                AND EXISTS (SELECT FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(c.tenant_id,c.order_id)
                            AND b.subject='order.order.accepted' AND b.published_at IS NOT NULL)
                AND EXISTS (SELECT FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(c.tenant_id,c.order_id)
                            AND b.subject='order.order.routed' AND b.published_at IS NULL)
                AND EXISTS (SELECT FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(c.tenant_id,c.order_id)
                            AND b.subject='order.order.partially_filled' AND b.published_at IS NULL
                            AND sha256(b.payload)=execution.fact_payload_sha256)
                AND EXISTS (SELECT FROM public.outbox b WHERE (b.tenant_id,b.partition_key)=(c.tenant_id,c.order_id)
                            AND b.subject='order.order.cancelled' AND b.published_at IS NULL
                            AND b.causation_id=cancel_source.event_id AND b.trace_context=cancel_source.trace_context
                            AND sha256(b.payload)=receipt.fact_payload_sha256)
                AND (SELECT count(*) FROM public.capital_commitments f WHERE (f.tenant_id,f.portfolio_id)=(c.tenant_id,c.portfolio_id))=
                    cardinality(funding.currencies)
                AND (SELECT count(*) FROM public.capital_members f WHERE f.tenant_id=c.tenant_id
                     AND (f.order_id=c.order_id OR f.owner_order_id=c.order_id))=cardinality(funding.currencies)
                AND NOT EXISTS (
                    SELECT FROM unnest(funding.currencies,funding.amounts,receipt.executed_debits) AS leg(currency,approved,debit)
                    WHERE NOT EXISTS (
                        SELECT FROM public.capital_commitments hold JOIN public.capital_members member USING(tenant_id,order_id,currency)
                        JOIN public.capital_balances cash USING(tenant_id,portfolio_id,currency)
                        WHERE (hold.tenant_id,hold.portfolio_id,hold.order_id,hold.currency)=(c.tenant_id,c.portfolio_id,c.order_id,leg.currency)
                          AND member.owner_order_id=c.order_id
                          AND public.capital_exact_parts(hold.admission_debit)=public.capital_exact_parts(leg.approved)
                          AND public.capital_exact_parts(hold.required_debit)=public.capital_exact_parts(leg.debit)
                          AND public.capital_exact_parts(hold.executed_debit)=public.capital_exact_parts(leg.debit)
                          AND public.capital_exact_parts(member.executed_debit)=public.capital_exact_parts(leg.debit)
                          AND public.capital_exact_parts(cash.reserved)=public.capital_exact_parts(leg.debit)
                          AND hold.booked_debit='0' AND member.booked_debit='0'
                          AND hold.allocated_debit='0' AND member.allocated_debit='0')))
    ) THEN
        RAISE EXCEPTION USING ERRCODE='P1533', MESSAGE='funded order is not ready for Go recovery';
    END IF;
END $$;
REVOKE ALL ON FUNCTION public.synthetic_oms_check_funded_go_handback(bigint) FROM PUBLIC;

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

    IF pg_has_role(NEW.runtime_oid,'kanz_synthetic_oms_go_funded_runtime','MEMBER') THEN
        PERFORM public.synthetic_oms_check_funded_go_handback(OLD.epoch);
        RETURN NEW;
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
