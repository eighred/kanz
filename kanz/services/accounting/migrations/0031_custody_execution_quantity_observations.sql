-- New and old writers share the balance/membership queue. Only explicit signed
-- measurements enter this append-only history; the legacy absence sweep cannot clear it.
CREATE TABLE custody_execution_quantity_observations (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    observation bigint GENERATED ALWAYS AS IDENTITY,
    break_id text NOT NULL,
    run_id text NOT NULL,
    statement_id text NOT NULL,
    entry_id text NOT NULL,
    portfolio_id text NOT NULL,
    custodian_id text NOT NULL,
    business_date date NOT NULL,
    account_id text NOT NULL,
    instrument_id text NOT NULL,
    external_ref text NOT NULL,
    source_revision bigint NOT NULL CHECK (source_revision > 0),
    ibor text NOT NULL,
    custodian text NOT NULL,
    difference text NOT NULL,
    first_seen_at timestamptz NOT NULL,
    measured_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, run_id, break_id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES custody_runs(tenant_id, run_id),
    FOREIGN KEY (tenant_id, statement_id) REFERENCES custody_batch_receipts(tenant_id, statement_id),
    FOREIGN KEY (tenant_id, entry_id) REFERENCES ledger_entries(tenant_id, entry_id)
);
CREATE INDEX custody_quantity_item_latest ON custody_execution_quantity_observations
    (tenant_id, portfolio_id, custodian_id, business_date, break_id, observation DESC);
ALTER TABLE custody_execution_quantity_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_execution_quantity_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_quantity_tenant ON custody_execution_quantity_observations
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
-- Refuse mismatched source, account or book attribution even if a writer supplies
-- a valid entry ID or statement ID belonging to another close in this tenant.
CREATE OR REPLACE FUNCTION custody_quantity_observation_origin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM custody_runs r
        JOIN custody_batch_receipts s USING (tenant_id,statement_id,portfolio_id,custodian_id,business_date)
        JOIN ledger_entries e ON e.tenant_id=r.tenant_id AND e.entry_id=NEW.entry_id AND e.portfolio_id=r.portfolio_id
        WHERE r.tenant_id=NEW.tenant_id AND r.run_id=NEW.run_id AND r.statement_id=NEW.statement_id
          AND r.portfolio_id=NEW.portfolio_id AND r.custodian_id=NEW.custodian_id AND r.business_date=NEW.business_date
          AND r.outcome IN ('clean','breaks') AND r.signed_statement_sha256=decode(s.sha256,'hex')
          AND r.book_basis->>'mode'='declared'
          AND r.book_basis->'accounts'->r.custodian_id=jsonb_build_array(s.account_id)
          AND EXISTS (SELECT FROM jsonb_array_elements(r.book_basis->'assignments') a
              WHERE a->>'account_id'=s.account_id AND a->>'custodian_id'=s.custodian_id AND a->>'portfolio_id'=s.portfolio_id)
          AND s.account_id=NEW.account_id AND s.revision=NEW.source_revision
          AND e.venue_account_id=NEW.account_id AND e.source_ref=NEW.external_ref AND e.instrument_id=NEW.instrument_id
          AND (coalesce(e.settlement_date,e.effective_time) AT TIME ZONE 'UTC')::date=NEW.business_date
          AND e.quantity IS NOT NULL
          AND split_part(e.quantity,'/',1)::numeric =
              NEW.ibor::numeric * coalesce(nullif(split_part(e.quantity,'/',2),''),'1')::numeric
          AND NEW.difference::numeric=NEW.ibor::numeric-NEW.custodian::numeric
    ) THEN
        RAISE EXCEPTION 'custody quantity observation lacks a corresponding signed account and booked trade';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_quantity_origin BEFORE INSERT ON custody_execution_quantity_observations
    FOR EACH ROW EXECUTE FUNCTION custody_quantity_observation_origin();
CREATE TRIGGER custody_quantity_observation_immutable BEFORE UPDATE OR DELETE ON custody_execution_quantity_observations
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_quantity_observation_no_truncate BEFORE TRUNCATE ON custody_execution_quantity_observations
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm();
