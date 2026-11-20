-- Accounting's durable cash-reservation handoff (#1316). Retained payloads
-- outlive JetStream retention; a transport gap must never invent coverage.
CREATE TABLE cash_commit_heads (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    observed_at_ns BIGINT,
    PRIMARY KEY (tenant_id, portfolio_id, currency)
);
CREATE TABLE cash_commit_debits (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    order_id TEXT NOT NULL,
    debit TEXT NOT NULL,
    PRIMARY KEY (tenant_id, portfolio_id, currency, order_id),
    FOREIGN KEY (tenant_id, portfolio_id, currency)
        REFERENCES cash_commit_heads (tenant_id, portfolio_id, currency)
);
CREATE TABLE cash_commit_history (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    payload BYTEA NOT NULL,
    PRIMARY KEY (tenant_id, portfolio_id, currency, revision),
    FOREIGN KEY (tenant_id, portfolio_id, currency)
        REFERENCES cash_commit_heads (tenant_id, portfolio_id, currency)
);
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['cash_commit_heads', 'cash_commit_debits', 'cash_commit_history'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = app_current_tenant())
        WITH CHECK (tenant_id = app_current_tenant())
    $f$, t);
  END LOOP;
END $$;
CREATE TRIGGER cash_commit_history_no_mutate
    BEFORE UPDATE OR DELETE ON cash_commit_history
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
