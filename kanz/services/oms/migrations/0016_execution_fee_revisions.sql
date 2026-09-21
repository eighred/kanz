-- Append-only fee revisions. Original execution evidence is never updated.
-- Shared contract with accounting's 0017; book distinguishes the two OMS books.
CREATE TABLE execution_fee_revisions (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    revision_sequence BIGSERIAL NOT NULL,
    book TEXT NOT NULL CHECK (book IN ('oms','position','ledger')),
    execution_key TEXT NOT NULL,
    order_id TEXT NOT NULL,
    case_id TEXT NOT NULL,
    approval_digest TEXT NOT NULL CHECK (length(approval_digest)=64),
    previous_fill BYTEA NOT NULL,
    revised_fill BYTEA NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id,book,execution_key,case_id)
);
CREATE INDEX execution_fee_head ON execution_fee_revisions (tenant_id,book,execution_key,revision_sequence DESC);
CREATE INDEX execution_fee_order_heads ON execution_fee_revisions (tenant_id,book,order_id,execution_key,revision_sequence DESC);
ALTER TABLE execution_fee_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_fee_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_fee_revisions
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE TRIGGER execution_fee_immutable BEFORE UPDATE OR DELETE ON execution_fee_revisions
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER execution_fee_no_truncate BEFORE TRUNCATE ON execution_fee_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
