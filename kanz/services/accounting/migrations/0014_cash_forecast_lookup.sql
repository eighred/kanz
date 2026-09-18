-- Bound a recorded-cash forecast within tenant, portfolio and currency before
-- applying its knowledge/effective-time predicates and row budget.
CREATE INDEX IF NOT EXISTS ledger_cash_forecast_lookup
    ON ledger_entries(tenant_id,portfolio_id,cash_currency,entry_id)
    WHERE cash IS NOT NULL;
