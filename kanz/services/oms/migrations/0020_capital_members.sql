-- A schedule reserves once at its parent. Individual child execution and
-- accounting identities still need separate cumulative debit heads: folding
-- every child's cumulative debit directly into the parent would overwrite its
-- siblings, while reserving each child independently would spend twice.
CREATE TABLE capital_members (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    order_id text NOT NULL,
    owner_order_id text NOT NULL,
    booked_debit text NOT NULL DEFAULT '0',
    executed_debit text NOT NULL DEFAULT '0',
    PRIMARY KEY (tenant_id, order_id),
    FOREIGN KEY (tenant_id, owner_order_id)
        REFERENCES capital_commitments (tenant_id, order_id)
);
CREATE INDEX capital_members_owner ON capital_members (tenant_id, owner_order_id);
ALTER TABLE capital_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE capital_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON capital_members
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());

-- Existing owners are materialized under their tenant and cash lock on first
-- use. A migration-time SELECT over FORCE RLS would only copy the migration
-- connection's tenant, silently omitting every other tenant's debit history.

CREATE FUNCTION reject_capital_member_reassignment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.order_id IS DISTINCT FROM OLD.order_id
        OR NEW.owner_order_id IS DISTINCT FROM OLD.owner_order_id THEN
        RAISE EXCEPTION 'capital commitment ownership is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER capital_member_immutable BEFORE UPDATE OR DELETE ON capital_members
    FOR EACH ROW EXECUTE FUNCTION reject_capital_member_reassignment();
