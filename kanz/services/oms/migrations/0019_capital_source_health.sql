-- An undecodable source fact has no trustworthy portfolio identity. Its fault
-- therefore blocks spending for this tenant, including portfolios first seen
-- AFTER the fault. A later healthy balance cannot erase contradictory evidence.
CREATE TABLE capital_source_health (
    tenant_id text PRIMARY KEY DEFAULT app_current_tenant(),
    fault_digest bytea CHECK (fault_digest IS NULL OR octet_length(fault_digest)=32)
);
ALTER TABLE capital_source_health ENABLE ROW LEVEL SECURITY;
ALTER TABLE capital_source_health FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON capital_source_health
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());

CREATE FUNCTION protect_capital_source_fault() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR (OLD.fault_digest IS NOT NULL AND
        NEW.fault_digest IS DISTINCT FROM OLD.fault_digest) THEN
        RAISE EXCEPTION 'capital source fault requires verified projection rebuild' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER capital_source_fault_immutable BEFORE UPDATE OR DELETE ON capital_source_health
    FOR EACH ROW EXECUTE FUNCTION protect_capital_source_fault();
