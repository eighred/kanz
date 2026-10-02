-- Cash events are a contiguous accounting sequence, not a last-arrival-wins
-- balance. A known gap blocks admission until its missing prefix is restored.
CREATE TABLE capital_balances (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    portfolio_id text NOT NULL,
    currency text NOT NULL,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    gap_revision bigint NOT NULL DEFAULT 0 CHECK (gap_revision >= 0),
    conflicted boolean NOT NULL DEFAULT false,
    total text NOT NULL DEFAULT '0',
    reserved text NOT NULL DEFAULT '0',
    observed_at_ns bigint,
    PRIMARY KEY (tenant_id, portfolio_id, currency)
);
CREATE TABLE capital_cash_events (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    portfolio_id text NOT NULL,
    currency text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    digest bytea NOT NULL CHECK (octet_length(digest)=32),
    PRIMARY KEY (tenant_id, portfolio_id, currency, revision)
);
CREATE TABLE capital_commitments (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    order_id text NOT NULL,
    portfolio_id text NOT NULL,
    currency text NOT NULL,
    required_debit text NOT NULL,
    booked_debit text NOT NULL DEFAULT '0',
    executed_debit text NOT NULL DEFAULT '0',
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    PRIMARY KEY (tenant_id, order_id),
    FOREIGN KEY (tenant_id, portfolio_id, currency)
        REFERENCES capital_balances (tenant_id, portfolio_id, currency)
);
CREATE INDEX capital_commitments_portfolio ON capital_commitments (tenant_id, portfolio_id, currency);
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['capital_balances','capital_cash_events','capital_commitments'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant())', t);
    END LOOP;
END $$;

CREATE FUNCTION reject_capital_receipt_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'capital accounting receipt identity is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER capital_receipt_immutable BEFORE UPDATE ON capital_cash_events
    FOR EACH ROW EXECUTE FUNCTION reject_capital_receipt_update();
