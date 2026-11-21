-- A historical comparison must not replace a later day's break lifecycle.
CREATE INDEX IF NOT EXISTS custody_runs_verified_date_idx
    ON custody_runs (tenant_id, portfolio_id, custodian_id, business_date)
    WHERE outcome IN ('clean', 'breaks');
