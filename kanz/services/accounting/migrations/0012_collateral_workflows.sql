-- Collateral control owns reservations; settlement remains an independent fact.
CREATE TABLE IF NOT EXISTS collateral_snapshots (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    snapshot_id TEXT NOT NULL,
    portfolio_id TEXT NOT NULL,
    as_of TIMESTAMPTZ NOT NULL,
    digest TEXT NOT NULL,
    payload BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,snapshot_id)
);
CREATE INDEX IF NOT EXISTS collateral_snapshot_time ON collateral_snapshots(tenant_id,portfolio_id,as_of DESC);
CREATE TABLE IF NOT EXISTS collateral_lots (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    lot_id TEXT NOT NULL,
    identity_digest TEXT NOT NULL,
    payload BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(tenant_id,lot_id)
);
CREATE TABLE IF NOT EXISTS collateral_workflows (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    workflow_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision>0),
    payload BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,workflow_id),
    -- One immutable obligation snapshot cannot be funded twice under new IDs.
    UNIQUE (tenant_id,snapshot_id),
    FOREIGN KEY (tenant_id,snapshot_id) REFERENCES collateral_snapshots(tenant_id,snapshot_id)
);
CREATE TABLE IF NOT EXISTS collateral_reservations (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    workflow_id TEXT NOT NULL,
    agreement_id TEXT NOT NULL,
    lot_id TEXT NOT NULL,
    quantity TEXT NOT NULL,
    PRIMARY KEY (tenant_id,workflow_id,agreement_id,lot_id),
    FOREIGN KEY (tenant_id,workflow_id) REFERENCES collateral_workflows(tenant_id,workflow_id)
);
CREATE INDEX IF NOT EXISTS collateral_reserved_lot ON collateral_reservations(tenant_id,lot_id);
CREATE TABLE IF NOT EXISTS collateral_requests (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    request_id TEXT NOT NULL,
    digest TEXT NOT NULL,
    evidence BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,request_id)
);
CREATE TABLE IF NOT EXISTS collateral_confirmations (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    source_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    agreement_id TEXT NOT NULL,
    lot_id TEXT NOT NULL,
    returned BOOLEAN NOT NULL,
    digest TEXT NOT NULL,
    payload BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,source_id),
    FOREIGN KEY (tenant_id,workflow_id) REFERENCES collateral_workflows(tenant_id,workflow_id)
);
CREATE TABLE IF NOT EXISTS collateral_active_agreements (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    agreement_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id,agreement_id),
    FOREIGN KEY (tenant_id,workflow_id) REFERENCES collateral_workflows(tenant_id,workflow_id)
);
DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['collateral_snapshots','collateral_workflows','collateral_reservations','collateral_requests','collateral_confirmations','collateral_active_agreements','collateral_lots'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',table_name);
        IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname=current_schema() AND tablename=table_name AND policyname='tenant_isolation') THEN
            EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant())',table_name);
        END IF;
    END LOOP;
END $$;
CREATE OR REPLACE FUNCTION refuse_collateral_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'collateral evidence is immutable'; END $$;
DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['collateral_snapshots','collateral_requests','collateral_confirmations'] LOOP
        EXECUTE format('CREATE OR REPLACE TRIGGER collateral_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION refuse_collateral_evidence_mutation()',table_name);
        EXECUTE format('CREATE OR REPLACE TRIGGER collateral_no_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION refuse_collateral_evidence_mutation()',table_name);
    END LOOP;
END $$;
