-- EXEC-M16 — THE LEDGER SEGREGATES COLLATERAL THE EXCHANGE DOES NOT.
--
-- ledger_entries has always kept cash per PORTFOLIO. An exchange does not: it
-- margins, nets and LIQUIDATES per ACCOUNT — the OKX sub-account, the Binance
-- account behind one API credential. If two portfolios settle into one exchange
-- account, they are one collateral pool no matter what this table says. A drawdown in
-- the first triggers a liquidation, the exchange sells whatever is in the account, and
-- the second portfolio's margin is gone — while its rows here still report the cash
-- sitting there. Those books are not imprecise. They are WRONG, and they are wrong in
-- the direction that loses money.
--
-- So an entry records the account it SETTLED AGAINST, and a write must declare which
-- account it is moving.
--
-- app_current_venue_account() is the declaration, and it RAISES when there is none:
--
--   · GUC never set        → ERROR (42501). Nobody said whose collateral this entry
--                            moves. An entry that cannot say that must not be written.
--   · GUC set to ''        → '' — a real declaration: this entry touches NO exchange
--                            account (a manual cash movement, a corporate action).
--   · GUC set to 'okx-1'   → 'okx-1'.
--
-- current_setting(...,true) returns NULL when the GUC was never set and '' when it was
-- set to '', which is exactly the distinction needed: "I declare none" is an answer;
-- "I forgot to declare" is not. This is the MT-01e stance applied to the second
-- tenancy dimension — an unscoped write ERRORS, it never silently lands somewhere.
--
-- WHY WRITES ONLY. The WITH CHECK clause binds INSERTs; the USING clause (visibility)
-- stays tenant-scoped. A NAV replay folds a portfolio's whole history and legitimately
-- spans its accounts, so requiring the GUC to READ would break the fold for no safety:
-- reads are not how collateral gets misposted. Writes are.

CREATE OR REPLACE FUNCTION app_current_venue_account() RETURNS text
LANGUAGE plpgsql STABLE AS $fn$
DECLARE a text;
BEGIN
    a := current_setting('app.venue_account_id', true);
    IF a IS NULL THEN
        RAISE EXCEPTION 'app.venue_account_id is not set: this write does not say which exchange account''s collateral it moves. Set it to the account (set_config(''app.venue_account_id'', ''okx-sub-1'', true)) or to '''' for an entry that touches no exchange account.'
            USING ERRCODE = '42501';
    END IF;
    RETURN a;
END;
$fn$;

-- Existing rows predate the column: they honestly have no account recorded, and ''
-- says exactly that. Add it permissively first so the backfill cannot fail, then make
-- the DEFAULT the guard so every FUTURE write must declare.
ALTER TABLE ledger_entries
    ADD COLUMN IF NOT EXISTS venue_account_id TEXT NOT NULL DEFAULT '';

ALTER TABLE ledger_entries
    ALTER COLUMN venue_account_id SET DEFAULT app_current_venue_account();

-- Per-account cash and position folds — "what is actually in okx-sub-1", which is the
-- only number the exchange agrees with.
CREATE INDEX IF NOT EXISTS ledger_entries_venue_account_idx
    ON ledger_entries (tenant_id, venue_account_id, effective_time, knowledge_time, entry_id);

-- Rewrite the isolation policy: reads stay tenant-scoped; a WRITE must land in the
-- account the transaction declared. Every policy on the table is dropped first — a
-- leftover permissive policy would be OR'd with this one and would restore the silent
-- misposting this exists to prevent.
DO $$
DECLARE pol RECORD;
BEGIN
    FOR pol IN SELECT policyname FROM pg_policies WHERE tablename = 'ledger_entries'
    LOOP
        EXECUTE format('DROP POLICY %I ON ledger_entries', pol.policyname);
    END LOOP;
END;
$$;

ALTER TABLE ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ledger_entries
    USING (tenant_id = app_current_tenant())
    WITH CHECK (
        tenant_id        = app_current_tenant()
        AND venue_account_id = app_current_venue_account()
    );
