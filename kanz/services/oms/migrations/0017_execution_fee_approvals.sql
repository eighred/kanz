CREATE TABLE execution_fee_proposals (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    order_id TEXT NOT NULL,
    digest TEXT NOT NULL CHECK (length(digest)=64),
    proposal BYTEA NOT NULL CHECK (octet_length(proposal)<=524288),
    PRIMARY KEY (tenant_id,case_id),
    FOREIGN KEY (tenant_id,case_id) REFERENCES execution_recovery_cases(tenant_id,case_id)
);
CREATE TABLE execution_fee_approvals (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    approval BYTEA NOT NULL,
    PRIMARY KEY (tenant_id,case_id),
    FOREIGN KEY (tenant_id,case_id) REFERENCES execution_fee_proposals(tenant_id,case_id)
);
ALTER TABLE execution_fee_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_fee_proposals FORCE ROW LEVEL SECURITY;
ALTER TABLE execution_fee_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_fee_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_fee_proposals USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE POLICY tenant_isolation ON execution_fee_approvals USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE TRIGGER execution_fee_proposals_immutable BEFORE UPDATE OR DELETE ON execution_fee_proposals FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER execution_fee_proposals_no_truncate BEFORE TRUNCATE ON execution_fee_proposals FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER execution_fee_approvals_immutable BEFORE UPDATE OR DELETE ON execution_fee_approvals FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER execution_fee_approvals_no_truncate BEFORE TRUNCATE ON execution_fee_approvals FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
