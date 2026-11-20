-- Reserve in the caller's transaction. Invoker rights and tenant RLS apply
-- to the existing ledger tables. Ordinary business refusals return without
-- aborting the transaction, so its OMS owner can still record the outcome.
CREATE FUNCTION capital_reduce_exact(n numeric, d numeric) RETURNS numeric[]
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE SET search_path=pg_catalog AS $$
DECLARE
    -- 2^512, the exclusive component bound in dec.Exact.Rat.
    bound CONSTANT numeric := 13407807929942597099574024998205846127479365820592393377723561443721764030073546976801874298166903427690031858186486050853753882811946569946433649006084096;
    factor numeric;
BEGIN
    IF d <= 0 OR n <> trunc(n) OR d <> trunc(d) THEN RETURN NULL; END IF;
    factor := gcd(n, d);
    n := div(n, factor);
    d := div(d, factor);
    IF abs(n) >= bound OR d >= bound THEN RETURN NULL; END IF;
    RETURN ARRAY[n,d];
END $$;

CREATE FUNCTION capital_exact_parts(value text) RETURNS numeric[]
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    digits text;
    n numeric;
    d numeric := 1;
    i integer;
BEGIN
    IF octet_length(value) > 400 OR value COLLATE "C" !~ '^-?[0-9]+([.][0-9]+|/[1-9][0-9]*)?$' THEN
        RETURN NULL;
    END IF;
    IF strpos(value, '/') > 0 THEN
        digits := ltrim(split_part(value, '/', 1), '-');
        d := split_part(value, '/', 2)::numeric;
        -- For slash-form input math/big.Rat reads the numerator as a base-0
        -- integer. A leading zero is octal here, but decimal in fixed-point form.
        IF length(digits) > 1 AND left(digits, 1) = '0' THEN
            IF digits COLLATE "C" ~ '[89]' THEN RETURN NULL; END IF;
            n := 0;
            FOR i IN 1..length(digits) LOOP
                n := n * 8 + substr(digits, i, 1)::integer;
            END LOOP;
        ELSE
            n := digits::numeric;
        END IF;
        IF left(value, 1) = '-' THEN n := -n; END IF;
    ELSE
        n := replace(value, '.', '')::numeric;
        d := ('1' || repeat('0', length(split_part(value, '.', 2))))::numeric;
    END IF;
    RETURN capital_reduce_exact(n, d);
END $$;

CREATE FUNCTION capital_reserve(
    p_portfolio text, p_currency text, p_order_id text, p_debit text,
    p_now_seconds bigint, p_now_nanos integer, p_max_age_ns bigint
) RETURNS text LANGUAGE plpgsql SECURITY INVOKER AS $$
DECLARE
    -- strings.TrimSpace uses Unicode White_Space. Identifier lengths are bytes.
    spaces CONSTANT text := U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000';
    tenant text;
    fault bytea;
    cash record;
    debit numeric[];
    total numeric[];
    reserved numeric[];
    next_reserved numeric[];
    next_n numeric;
    next_d numeric;
    age_ns numeric;
    debit_text text;
BEGIN
    IF EXISTS (SELECT FROM unnest(ARRAY[p_portfolio,p_currency,p_order_id]) AS ids(id)
               WHERE id IS NULL OR octet_length(id)>256 OR btrim(id, spaces)='')
       OR p_now_seconds IS NULL OR p_now_nanos IS NULL OR p_now_nanos NOT BETWEEN 0 AND 999999999
       OR (p_now_seconds=-62135596800 AND p_now_nanos=0)
       OR p_max_age_ns IS NULL OR p_max_age_ns<=0 THEN
        RETURN 'INVALID';
    END IF;
    debit := capital_exact_parts(p_debit);
    IF debit IS NULL OR debit[1]<0 THEN RETURN 'INVALID'; END IF;

    tenant := app_current_tenant();
    SELECT fault_digest INTO fault FROM capital_source_health WHERE tenant_id=tenant FOR SHARE;
    IF NOT FOUND OR fault IS NOT NULL THEN RETURN 'UNKNOWN'; END IF;

    SELECT revision,gap_revision,conflicted,coverage_complete,observed_at_ns,
           b.total,b.reserved INTO cash FROM capital_balances b
        WHERE b.tenant_id=tenant AND b.portfolio_id=p_portfolio AND b.currency=p_currency FOR UPDATE;
    IF NOT FOUND THEN RETURN 'UNKNOWN'; END IF;
    total := capital_exact_parts(cash.total);
    reserved := capital_exact_parts(cash.reserved);
    IF total IS NULL OR reserved IS NULL OR reserved[1]<0 THEN RETURN 'INVALID'; END IF;

    -- now can be outside int64 nanoseconds. Go time.Sub saturates at MaxInt64.
    age_ns := p_now_seconds::numeric*1000000000 + p_now_nanos - cash.observed_at_ns;
    IF NOT cash.coverage_complete OR cash.conflicted OR cash.revision<=0 OR cash.gap_revision>cash.revision
       OR age_ns IS NULL OR age_ns<0 OR (p_max_age_ns<9223372036854775807 AND age_ns>p_max_age_ns) THEN
        RETURN 'UNKNOWN';
    END IF;
    next_n := reserved[1]*debit[2] + debit[1]*reserved[2];
    next_d := reserved[2]*debit[2];
    IF next_n*total[2] > total[1]*next_d THEN RETURN 'INSUFFICIENT'; END IF;
    next_reserved := capital_reduce_exact(next_n, next_d);
    IF next_reserved IS NULL THEN RETURN 'INVALID'; END IF;

    debit_text := debit[1]::text || CASE WHEN debit[2]=1 THEN '' ELSE '/' || debit[2]::text END;
    INSERT INTO capital_commitments(order_id,portfolio_id,currency,required_debit,admission_debit)
        VALUES(p_order_id,p_portfolio,p_currency,debit_text,debit_text) ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN RETURN 'CONFLICT'; END IF;
    INSERT INTO capital_members(order_id,owner_order_id,currency) VALUES(p_order_id,p_order_id,p_currency);
    IF NOT FOUND THEN RAISE EXCEPTION 'missing reservation membership write'; END IF;
    UPDATE capital_balances SET reserved=next_reserved[1]::text ||
            CASE WHEN next_reserved[2]=1 THEN '' ELSE '/' || next_reserved[2]::text END
        WHERE tenant_id=tenant AND portfolio_id=p_portfolio AND currency=p_currency;
    IF NOT FOUND THEN RAISE EXCEPTION 'missing reservation balance write'; END IF;
    RETURN 'RESERVED';
END $$;

-- These invokers can also be called from a restricted trigger. Resolve
-- relations/functions in the installed ledger schema independently of callers.
DO $$
DECLARE
    ledger name := current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.capital_exact_parts(text) SET search_path TO pg_catalog,%I,pg_temp', ledger, ledger);
    EXECUTE format('ALTER FUNCTION %I.capital_reserve(text,text,text,text,bigint,integer,bigint) SET search_path TO pg_catalog,%I,pg_temp', ledger, ledger);
END $$;
