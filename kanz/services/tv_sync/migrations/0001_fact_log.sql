-- 0001: tv-sync's book was LOST on every restart (EXEC-M21).
--
-- tv-sync folds order-lifecycle FACTs into memory, and the TradingView Broker API serves
-- the fund's orders, executions, positions and P&L straight out of it. There was NO
-- DATABASE anywhere in the service, and its bus consumer is a DURABLE GROUP: a restarted
-- pod resumes at its last ack and never re-reads what it already folded.
--
-- So a pod roll left the trader looking at an EMPTY ACCOUNT — no positions, no orders,
-- zero P&L — while the fund's real positions sat open at the exchanges.
--
-- And there was no way back. The EXECUTION stream that carries `order.>` has a 24h
-- max-age, nothing archives it, and no table on this platform persists a fill
-- (`position_fills` holds only fill_ids, for the exactly-once claim). Replaying the stream
-- from its start would rebuild AT MOST YESTERDAY, and realized P&L since inception was
-- simply gone.
--
-- # This table holds FACTS, not a book
--
-- tv-sync is zero-truth: it derives everything it serves from folded FACTs and holds no
-- independent state. A table of orders, executions and positions would make it a SECOND
-- BOOK — one that could disagree with the OMS's, with nobody able to say which was right.
--
-- So what is durable here is the FACT LOG itself: the events tv-sync folded, byte for byte,
-- in the order it folded them. A booting pod replays them through the SAME fold and arrives
-- at the SAME book. The store originates nothing and answers no query; it cannot drift from
-- the truth because it holds no opinion about it.
--
-- The OMS's `orders` table remains the book of record for orders, and accounting's ledger
-- for the fund's economics. This is a projection's private write-ahead of its own input.

CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $fn$
DECLARE t text;
BEGIN
  t := current_setting('app.tenant_id', true);
  IF t IS NULL OR t = '' THEN
    RAISE EXCEPTION 'kanz: tenant scope missing — app.tenant_id is not set on this session'
      USING ERRCODE = '42501',
            HINT = 'every connection must set app.tenant_id (pgxpool AfterConnect); see internal/pg.NewTenantPool';
  END IF;
  RETURN t;
END $fn$;

CREATE TABLE tv_facts (
    tenant_id    TEXT        NOT NULL DEFAULT app_current_tenant(),
    -- seq is the order tv-sync FOLDED these facts, and replay follows it exactly. The fold
    -- is order-dependent — an average-cost basis depends on which fill came first — so a
    -- rebuild that reordered them would produce a DIFFERENT book from the one the pod had,
    -- which is a subtler failure than losing it. event_id is a UUIDv7 and roughly sorts by
    -- emission time, but "roughly" is not "identically", and this is a capital path.
    seq          BIGSERIAL   NOT NULL,
    -- The envelope's event_id (UUIDv7): the identity that makes the fold exactly-once. The
    -- bus redelivers (an ack lost to a crash, a pod that died between folding and acking),
    -- and a fill folded twice doubles a position the fund does not hold.
    event_id     TEXT        NOT NULL,
    event_type   TEXT        NOT NULL,
    -- The FACT as it arrived on the wire. Not a parse of it: a schema that gains a field
    -- must not silently drop it from every rebuild thereafter.
    payload      BYTEA       NOT NULL,
    -- The KNOWLEDGE time of the original fold, not of the replay. The projection is
    -- bitemporal — its Broker API answers "as known at T" — so a rebuild that stamped
    -- today's clock on a fill from last month would move history.
    knowledge_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, event_id)
);

-- Replay reads one tenant's log in fold order; the PK does not serve that.
CREATE INDEX tv_facts_seq_idx ON tv_facts (tenant_id, seq);

-- Tenant isolation (MT-01d/e). app_current_tenant() RAISES when the GUC is unset, so an
-- unscoped session gets an ERROR rather than a silently empty log — and a silently empty
-- log rebuilds a silently empty book, which is exactly the failure this table exists to end.
ALTER TABLE tv_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE tv_facts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tv_facts
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
