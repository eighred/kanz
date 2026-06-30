-- 0001: durable household composition (PARITY-02b).
--
-- The household book is a current-state projection (WEALTH-01b), not an event
-- journal: Put is last-write-wins on household_id. The bus consumer (PARITY-02e)
-- folds live account/holding state into it, and internal/wealth aggregates it
-- into the virtual portfolio for the household-level exposure/risk view. The
-- durable store changes nothing about the aggregation.
--
-- The household is a pure-float analytics shape (weights/market value are derived
-- statistics, the EVT-14 float-at-the-analytics-edge rule), so it is stored as a
-- JSONB blob keyed by household id — the natural shape for a replace-on-write
-- projection.

CREATE TABLE households (
    tenant_id    TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    household_id TEXT        NOT NULL,
    composition  JSONB       NOT NULL,            -- the wealth.Household snapshot
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, household_id)
);

-- Tenant isolation via row-level security (MT-01d): deny-by-default.
ALTER TABLE households ENABLE ROW LEVEL SECURITY;
ALTER TABLE households FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON households
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
