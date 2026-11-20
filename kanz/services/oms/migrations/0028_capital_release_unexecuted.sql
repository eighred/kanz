-- The caller must first prove a terminal cancellation of a direct, unfilled,
-- never-dispatched order and bind the retained funding approval to its command.
-- Its order CAS, receipt and FACT commit in the same transaction as this call.
-- This ledger operation has no authority to decide that an order is terminal.
CREATE FUNCTION capital_release_unexecuted(
    p_portfolio text, p_order_id text, p_currencies text[], p_admission_debits text[]
) RETURNS text LANGUAGE plpgsql SECURITY INVOKER AS $$
DECLARE
    spaces CONSTANT text := U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000';
    zero CONSTANT numeric[] := ARRAY[0,1]::numeric[];
    tenant text;
    legs integer := cardinality(p_currencies);
    i integer;
    debit numeric[];
    cash record;
    retained record;
    reserved numeric[];
    remainder numeric[];
    remaining_text text[] := '{}';
    changed integer;
BEGIN
    IF legs IS NULL OR legs NOT BETWEEN 1 AND 32 OR cardinality(p_admission_debits) IS DISTINCT FROM legs
       OR array_ndims(p_currencies) IS DISTINCT FROM 1 OR array_ndims(p_admission_debits) IS DISTINCT FROM 1
       OR array_lower(p_currencies,1)<>1 OR array_lower(p_admission_debits,1)<>1
       OR EXISTS (SELECT FROM unnest(ARRAY[p_portfolio,p_order_id] || p_currencies) AS ids(id)
                  WHERE id IS NULL OR octet_length(id)>256 OR btrim(id,spaces)='') THEN
        RETURN 'INVALID';
    END IF;

    -- Check the approval before taking any locks. Its strictly increasing
    -- bytewise currency order is also the order used by ReserveMany.
    FOR i IN 1..legs LOOP
        debit := capital_exact_parts(p_admission_debits[i]);
        IF debit IS NULL OR debit[1]<0 OR (i>1 AND p_currencies[i-1] COLLATE "C">=p_currencies[i]) THEN
            RETURN 'INVALID';
        END IF;
    END LOOP;

    tenant := app_current_tenant();
    PERFORM tenant_id FROM capital_source_health WHERE tenant_id=tenant FOR SHARE;
    IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
    -- A confirmed reduction is permitted on stale or faulted cash. Like
    -- LockBalances, require the existing, parseable balance head on every leg.
    -- Acquire ALL cash locks before reading child memberships or commitment
    -- heads, including fee currencies with a zero authorization.
    FOR i IN 1..legs LOOP
        SELECT b.total,b.reserved INTO cash FROM capital_balances b
            WHERE b.tenant_id=tenant AND b.portfolio_id=p_portfolio AND b.currency=p_currencies[i] FOR UPDATE;
        IF NOT FOUND OR capital_exact_parts(cash.total) IS NULL THEN RETURN 'UNKNOWN'; END IF;
        reserved := capital_exact_parts(cash.reserved);
        debit := capital_exact_parts(p_admission_debits[i]);
        IF reserved IS NULL OR reserved[1]<0 THEN RETURN 'UNKNOWN'; END IF;
        -- Change's exact reserved - max(required-booked,0) for a zero-execution
        -- cancellation. Below we require that the original, unbooked debit is
        -- still both the required head and the independently approved amount.
        remainder := capital_reduce_exact(reserved[1]*debit[2] - debit[1]*reserved[2], reserved[2]*debit[2]);
        IF remainder IS NULL OR remainder[1]<0 THEN RETURN 'UNKNOWN'; END IF;
        remaining_text := array_append(remaining_text, remainder[1]::text ||
            CASE WHEN remainder[2]=1 THEN '' ELSE '/' || remainder[2]::text END);
    END LOOP;

    IF EXISTS (SELECT FROM capital_commitments WHERE tenant_id=tenant AND order_id=p_order_id
               AND (portfolio_id<>p_portfolio OR currency<>ALL(p_currencies)))
       OR EXISTS (SELECT FROM capital_members WHERE tenant_id=tenant AND (order_id=p_order_id OR owner_order_id=p_order_id)
               AND (order_id<>p_order_id OR owner_order_id<>p_order_id OR currency<>ALL(p_currencies))) THEN
        RETURN 'UNKNOWN';
    END IF;
    FOR i IN 1..legs LOOP
        SELECT c.version,c.admission_debit,c.required_debit,c.allocated_debit,c.booked_debit,c.executed_debit,
               m.allocated_debit AS member_allocated,m.booked_debit AS member_booked,m.executed_debit AS member_executed
            INTO retained FROM capital_commitments c JOIN capital_members m USING(tenant_id,order_id,currency)
            WHERE c.tenant_id=tenant AND c.order_id=p_order_id AND c.portfolio_id=p_portfolio
              AND c.currency=p_currencies[i] AND m.owner_order_id=c.order_id FOR UPDATE OF c;
        IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
        debit := capital_exact_parts(p_admission_debits[i]);
        IF retained.version<>1 OR capital_exact_parts(retained.admission_debit) IS DISTINCT FROM debit
           OR capital_exact_parts(retained.required_debit) IS DISTINCT FROM debit
           OR capital_exact_parts(retained.allocated_debit) IS DISTINCT FROM zero
           OR capital_exact_parts(retained.booked_debit) IS DISTINCT FROM zero
           OR capital_exact_parts(retained.executed_debit) IS DISTINCT FROM zero
           OR capital_exact_parts(retained.member_allocated) IS DISTINCT FROM zero
           OR capital_exact_parts(retained.member_booked) IS DISTINCT FROM zero
           OR capital_exact_parts(retained.member_executed) IS DISTINCT FROM zero THEN
            RETURN 'UNKNOWN';
        END IF;
    END LOOP;

    -- Keep admission and member heads for reconciliation. A retry sees a
    -- changed version and cannot subtract again, even for a zero-debit leg.
    UPDATE capital_commitments SET required_debit='0', version=version+1
        WHERE tenant_id=tenant AND portfolio_id=p_portfolio AND order_id=p_order_id AND currency=ANY(p_currencies) AND version=1;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed<>legs THEN RAISE EXCEPTION 'incomplete terminal commitment update'; END IF;
    UPDATE capital_balances b SET reserved=x.remaining FROM unnest(p_currencies,remaining_text) AS x(currency,remaining)
        WHERE b.tenant_id=tenant AND b.portfolio_id=p_portfolio AND b.currency=x.currency;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed<>legs THEN RAISE EXCEPTION 'incomplete terminal balance update'; END IF;
    RETURN 'RELEASED';
END $$;

DO $$
DECLARE ledger name := current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.capital_release_unexecuted(text,text,text[],text[]) SET search_path TO pg_catalog,%I,pg_temp', ledger, ledger);
END $$;
