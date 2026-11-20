-- The order owner must claim the unique execution or confirmed terminal
-- transition in this transaction and independently bind these vectors to it.
-- This bounded case covers one direct fill. Scheduled orders, corrections and
-- additional executions require their own fully identified transition.
CREATE FUNCTION capital_settle_direct(
    p_portfolio text, p_order_id text, p_currencies text[], p_approved text[],
    p_executed text[], p_terminal boolean
) RETURNS text LANGUAGE plpgsql SECURITY INVOKER AS $$
DECLARE
    spaces CONSTANT text := U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000';
    zero CONSTANT numeric[] := ARRAY[0,1]::numeric[];
    tenant text;
    legs integer := cardinality(p_currencies);
    i integer;
    approved numeric[];
    actual numeric[];
    booked numeric[];
    peak numeric[];
    prior_required numeric[];
    required numeric[];
    reserved numeric[];
    next_reserved numeric[];
    head record;
    prior_executed numeric[];
    version_expected integer;
    has_execution boolean := false;
    required_text text[] := '{}';
    actual_text text[] := '{}';
    reserved_text text[] := '{}';
BEGIN
    IF p_terminal IS NULL OR legs IS NULL OR legs NOT BETWEEN 1 AND 32
       OR cardinality(p_approved) IS DISTINCT FROM legs OR cardinality(p_executed) IS DISTINCT FROM legs
       OR array_ndims(p_currencies) IS DISTINCT FROM 1 OR array_ndims(p_approved) IS DISTINCT FROM 1
       OR array_ndims(p_executed) IS DISTINCT FROM 1 OR array_lower(p_currencies,1)<>1
       OR array_lower(p_approved,1)<>1 OR array_lower(p_executed,1)<>1
       OR EXISTS (SELECT FROM unnest(ARRAY[p_portfolio,p_order_id] || p_currencies) AS ids(id)
                  WHERE id IS NULL OR octet_length(id)>256 OR btrim(id,spaces)='') THEN
        RETURN 'INVALID';
    END IF;
    FOR i IN 1..legs LOOP
        approved := capital_exact_parts(p_approved[i]);
        actual := capital_exact_parts(p_executed[i]);
        IF approved IS NULL OR approved[1]<0 OR actual IS NULL OR actual[1]<0
           OR (i>1 AND p_currencies[i-1] COLLATE "C">=p_currencies[i]) THEN RETURN 'INVALID'; END IF;
        has_execution := has_execution OR actual[1]>0;
    END LOOP;
    IF NOT has_execution THEN RETURN 'INVALID'; END IF;

    tenant := app_current_tenant();
    PERFORM tenant_id FROM capital_source_health WHERE tenant_id=tenant FOR SHARE;
    IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
    -- Acquire all cash locks in Go LockBalances order before any member lock.
    -- A reported fill remains a liability even if its cash source is now stale.
    FOR i IN 1..legs LOOP
        PERFORM 1 FROM capital_balances b WHERE b.tenant_id=tenant AND b.portfolio_id=p_portfolio
            AND b.currency=p_currencies[i] FOR UPDATE;
        IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
    END LOOP;
    IF EXISTS (SELECT FROM capital_commitments WHERE tenant_id=tenant AND order_id=p_order_id
               AND (portfolio_id<>p_portfolio OR currency<>ALL(p_currencies)))
       OR EXISTS (SELECT FROM capital_members WHERE tenant_id=tenant AND (order_id=p_order_id OR owner_order_id=p_order_id)
               AND (order_id<>p_order_id OR owner_order_id<>p_order_id OR currency<>ALL(p_currencies))) THEN
        RETURN 'UNKNOWN';
    END IF;
    FOR i IN 1..legs LOOP
        SELECT b.total,b.reserved,c.version,c.admission_debit,c.required_debit,c.booked_debit,c.executed_debit,
               c.allocated_debit,m.allocated_debit AS member_allocated,m.booked_debit AS member_booked,
               m.executed_debit AS member_executed INTO head
            FROM capital_commitments c JOIN capital_members m USING(tenant_id,order_id,currency)
            JOIN capital_balances b USING(tenant_id,portfolio_id,currency)
            WHERE c.tenant_id=tenant AND c.portfolio_id=p_portfolio AND c.order_id=p_order_id
                AND c.currency=p_currencies[i] AND m.owner_order_id=c.order_id FOR UPDATE OF c,m;
        IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
        approved := capital_exact_parts(p_approved[i]);
        actual := capital_exact_parts(p_executed[i]);
        peak := CASE WHEN actual[1]*approved[2]>approved[1]*actual[2] THEN actual ELSE approved END;
        prior_required := CASE WHEN p_terminal THEN peak ELSE approved END;
        required := CASE WHEN p_terminal THEN actual ELSE peak END;
        prior_executed := CASE WHEN p_terminal THEN actual ELSE zero END;
        version_expected := CASE WHEN p_terminal AND actual[1]>0 THEN 2 ELSE 1 END;
        booked := capital_exact_parts(head.booked_debit);
        reserved := capital_exact_parts(head.reserved);
        -- A single immutable fill has either no accounting receipt yet or the
        -- entire attributed debit. The accounting owner alone writes booked.
        IF head.version<>version_expected OR capital_exact_parts(head.admission_debit) IS DISTINCT FROM approved
           OR capital_exact_parts(head.required_debit) IS DISTINCT FROM prior_required
           OR capital_exact_parts(head.executed_debit) IS DISTINCT FROM prior_executed
           OR capital_exact_parts(head.member_executed) IS DISTINCT FROM prior_executed
           OR capital_exact_parts(head.allocated_debit) IS DISTINCT FROM zero
           OR capital_exact_parts(head.member_allocated) IS DISTINCT FROM zero
           OR booked IS NULL OR (booked<>zero AND (NOT p_terminal OR booked<>actual))
           OR capital_exact_parts(head.member_booked) IS DISTINCT FROM booked
           OR capital_exact_parts(head.total) IS NULL OR reserved IS NULL OR reserved[1]<0 THEN
            RETURN 'UNKNOWN';
        END IF;
        -- Both required heads cover booked. Require this order's complete
        -- unbooked hold to exist before adjusting aggregate reserved. The
        -- booked terms then cancel in reserved - (prior-booked) + (new-booked).
        IF reserved[1]*prior_required[2]*booked[2] <
               reserved[2]*(prior_required[1]*booked[2]-booked[1]*prior_required[2]) THEN
            RETURN 'UNKNOWN';
        END IF;
        next_reserved := capital_reduce_exact(
            reserved[1]*prior_required[2]*required[2] +
                reserved[2]*(required[1]*prior_required[2]-prior_required[1]*required[2]),
            reserved[2]*prior_required[2]*required[2]);
        IF next_reserved IS NULL THEN RETURN 'UNKNOWN'; END IF;
        required_text := array_append(required_text, required[1]::text || CASE WHEN required[2]=1 THEN '' ELSE '/' || required[2]::text END);
        actual_text := array_append(actual_text, actual[1]::text || CASE WHEN actual[2]=1 THEN '' ELSE '/' || actual[2]::text END);
        reserved_text := array_append(reserved_text, next_reserved[1]::text || CASE WHEN next_reserved[2]=1 THEN '' ELSE '/' || next_reserved[2]::text END);
    END LOOP;

    -- An unchanged zero-delta leg keeps its original version on a fill.
    -- FinishExecution advances every leg on confirmed terminal, even at zero.
    FOR i IN 1..legs LOOP
        IF NOT p_terminal AND actual_text[i]='0' THEN CONTINUE; END IF;
        IF NOT p_terminal THEN
            UPDATE capital_members SET executed_debit=actual_text[i] WHERE tenant_id=tenant
                AND order_id=p_order_id AND owner_order_id=p_order_id AND currency=p_currencies[i];
            IF NOT FOUND THEN RAISE EXCEPTION 'missing execution member update'; END IF;
        END IF;
        UPDATE capital_commitments SET required_debit=required_text[i],executed_debit=actual_text[i],version=version+1
            WHERE tenant_id=tenant AND portfolio_id=p_portfolio AND order_id=p_order_id AND currency=p_currencies[i];
        IF NOT FOUND THEN RAISE EXCEPTION 'missing execution commitment update'; END IF;
        UPDATE capital_balances SET reserved=reserved_text[i] WHERE tenant_id=tenant
            AND portfolio_id=p_portfolio AND currency=p_currencies[i];
        IF NOT FOUND THEN RAISE EXCEPTION 'missing execution balance update'; END IF;
    END LOOP;
    RETURN 'APPLIED';
END $$;

DO $$
DECLARE ledger name := current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.capital_settle_direct(text,text,text[],text[],text[],boolean) SET search_path TO pg_catalog,%I,pg_temp', ledger, ledger);
END $$;
