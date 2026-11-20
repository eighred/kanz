-- Every imported historical obligation retains the exact OMS state, posted
-- executions and authority version used to establish it. An audit can compare
-- this digest to the approved source without trusting a mutable application log.
CREATE TABLE capital_legacy_evidence (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    order_id text NOT NULL,
    source text NOT NULL,
    source_version text NOT NULL,
    approved_by text NOT NULL,
    evidence_digest bytea NOT NULL CHECK (octet_length(evidence_digest)=32),
    reconciled_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, order_id),
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders (tenant_id, order_id)
);
ALTER TABLE capital_legacy_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE capital_legacy_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON capital_legacy_evidence
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE TRIGGER capital_legacy_evidence_immutable BEFORE UPDATE OR DELETE ON capital_legacy_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_capital_receipt_update();
