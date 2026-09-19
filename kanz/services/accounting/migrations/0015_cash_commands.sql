CREATE TABLE IF NOT EXISTS cash_commands (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    movement_id TEXT NOT NULL,
    portfolio_id TEXT NOT NULL,
    digest TEXT NOT NULL,
    actor TEXT NOT NULL,
    command JSONB NOT NULL,
    receipt JSONB NOT NULL,
    payload BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,movement_id)
);
ALTER TABLE cash_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE cash_commands FORCE ROW LEVEL SECURITY;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname=current_schema() AND tablename='cash_commands' AND policyname='tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON cash_commands USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
    END IF;
END $$;
CREATE OR REPLACE FUNCTION refuse_cash_command_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'accepted cash commands are immutable'; END $$;
CREATE OR REPLACE TRIGGER cash_command_immutable BEFORE UPDATE OR DELETE ON cash_commands FOR EACH ROW EXECUTE FUNCTION refuse_cash_command_mutation();
CREATE OR REPLACE TRIGGER cash_command_no_truncate BEFORE TRUNCATE ON cash_commands FOR EACH STATEMENT EXECUTE FUNCTION refuse_cash_command_mutation();
