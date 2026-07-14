-- 0003: the durable position book (EXEC-M18).
--
-- THE BOOK WAS A MAP IN ONE PROCESS, AND THE OMS RUNS TWO PODS.
--
-- position.Book was `map[key]*lot` behind a mutex, and the projector consumed fill
-- FACTs through a durable consumer GROUP — which LOAD-BALANCES. oms-deploy.yaml ships
-- `replicas: 2`. So each pod folded only the fills it happened to receive, held a
-- PARTIAL book, and published domain.v1.PositionState as an ABSOLUTE quantity.
--
-- That is worse than a control that fails, because a projection does not merely fail to
-- act — it SPEAKS. The risk engine, the compliance monitor and tv-sync all believed
-- those numbers, and so did the OMS's own PRE-TRADE GATE, which projects every order
-- onto this book before admitting it: a concentration limit evaluated against half the
-- fund's positions does not refuse loudly, it quietly says yes. And on restart the map
-- came back EMPTY, so the next 0.1 BTC fill published `position = 0.1` while the fund
-- held 5.1.
--
-- EXEC-M7c made the ORDER store durable and declared `replicas: 2` safe. It made the
-- admission gate safe. It left the position book exactly where it was.
--
-- # Exactly-once, at the engine
--
-- A fill must fold ONCE — not once per pod, and not again on a redelivery. position_fills
-- is that guarantee, and it is the same stance as the orders table's admission gate: an
-- INSERT ... ON CONFLICT DO NOTHING whose RowsAffected the writer reads. 1 ⇒ this
-- delivery owns the fill and folds it; 0 ⇒ somebody already did, and the fold must be
-- skipped. A SELECT-then-INSERT would reintroduce the check-then-act window.
--
-- The lot row is then taken FOR UPDATE, so two pods folding the same instrument
-- serialize at the engine rather than racing in two address spaces.
--
-- # Exact decimals
--
-- quantity, average_price and realized_pnl are money. `double` is banned on any path
-- that moves capital (KANZ_BRAIN, EVT-14), and there is no lossless native SQL column
-- for an exact base-10 rational — so they are stored as the exact RatString, TEXT. That
-- is the stance accounting's ledger already takes (ledger.quantity/price) and the one
-- DATA-M8b converted the pricing path to. NUMERIC would be lossy at the boundary and
-- would invite arithmetic in SQL; the weighted-average-cost fold stays in Go, where it
-- is already tested.

CREATE TABLE positions (
    tenant_id     TEXT        NOT NULL DEFAULT app_current_tenant(),
    portfolio_id  TEXT        NOT NULL,
    instrument_id TEXT        NOT NULL,
    quantity      TEXT        NOT NULL,  -- exact RatString, signed: + long, - short
    average_price TEXT        NOT NULL,  -- exact RatString, cost of the open lot
    realized_pnl  TEXT        NOT NULL,  -- exact RatString, cumulative
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, portfolio_id, instrument_id)
);

-- The pre-trade gate reads a whole portfolio's holdings on every order admission.
CREATE INDEX positions_portfolio_idx ON positions (tenant_id, portfolio_id);

-- The fills already folded. This is what makes the fold exactly-once ACROSS PODS: the
-- primary key is the whole guarantee.
CREATE TABLE position_fills (
    tenant_id  TEXT        NOT NULL DEFAULT app_current_tenant(),
    fill_id    TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, fill_id)
);

-- Tenant isolation (MT-01d/e). app_current_tenant() RAISES when the GUC is unset, so an
-- unscoped session gets an ERROR rather than a silently empty book — and a silently empty
-- book is precisely the failure this migration exists to end.
ALTER TABLE positions ENABLE ROW LEVEL SECURITY;
ALTER TABLE positions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON positions
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE position_fills ENABLE ROW LEVEL SECURITY;
ALTER TABLE position_fills FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON position_fills
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
