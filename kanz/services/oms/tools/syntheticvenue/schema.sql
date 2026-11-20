-- Installed only in the disposable venue database by the synthetic slice harness.
CREATE SCHEMA synthetic_venue;
CREATE TABLE synthetic_venue.receipts (
    tenant_id text NOT NULL,
    portfolio_id text NOT NULL,
    order_id text NOT NULL,
    terms bytea NOT NULL,
    execution bytea,
    PRIMARY KEY (tenant_id, portfolio_id, order_id)
);
ALTER TABLE synthetic_venue.receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE synthetic_venue.receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY fixture_tenant ON synthetic_venue.receipts
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
REVOKE ALL ON SCHEMA synthetic_venue FROM PUBLIC;
GRANT USAGE ON SCHEMA synthetic_venue TO kanz_synthetic_venue;
GRANT SELECT, INSERT, UPDATE ON synthetic_venue.receipts TO kanz_synthetic_venue;
