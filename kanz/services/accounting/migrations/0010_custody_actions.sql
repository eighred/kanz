-- #1188: operator transitions and their evidence share one transaction. The
-- revision also advances on reconciliation, including resolution/reopening.
ALTER TABLE custody_breaks ADD COLUMN revision BIGINT NOT NULL DEFAULT 1
    CHECK (revision > 0);

CREATE OR REPLACE FUNCTION advance_custody_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.revision := OLD.revision + 1;
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_break_revision BEFORE UPDATE ON custody_breaks
    FOR EACH ROW EXECUTE FUNCTION advance_custody_revision();

CREATE TABLE custody_actions (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    request_id TEXT NOT NULL,
    break_id TEXT NOT NULL,
    actor TEXT NOT NULL CHECK (length(btrim(actor)) > 0),
    request_digest TEXT NOT NULL,
    evidence JSONB NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, request_id),
    FOREIGN KEY (tenant_id, break_id) REFERENCES custody_breaks (tenant_id, break_id)
);
ALTER TABLE custody_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_actions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON custody_actions
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

CREATE OR REPLACE FUNCTION refuse_custody_action_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'custody action evidence is immutable';
END;
$$;
CREATE TRIGGER custody_action_immutable BEFORE UPDATE OR DELETE ON custody_actions
    FOR EACH ROW EXECUTE FUNCTION refuse_custody_action_mutation();
CREATE TRIGGER custody_action_no_truncate BEFORE TRUNCATE ON custody_actions
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_custody_action_mutation();
