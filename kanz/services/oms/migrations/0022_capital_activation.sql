-- Activation is a tenant-scoped write barrier. Every order INSERT takes a
-- shared lock on this row; activation takes its exclusive lock and checks the
-- legacy population. An old OMS pod cannot race an unfunded order through the
-- scan or continue writing unfunded orders after activation commits.
CREATE TABLE capital_activation (
    tenant_id text PRIMARY KEY DEFAULT app_current_tenant(),
    enabled boolean NOT NULL DEFAULT false,
    activated_at timestamptz,
    CHECK ((enabled AND activated_at IS NOT NULL) OR (NOT enabled AND activated_at IS NULL))
);
ALTER TABLE capital_activation ENABLE ROW LEVEL SECURITY;
ALTER TABLE capital_activation FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON capital_activation
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());

CREATE FUNCTION keep_capital_activation_monotonic() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR (OLD.enabled AND (NOT NEW.enabled OR NEW.activated_at IS DISTINCT FROM OLD.activated_at)) THEN
        RAISE EXCEPTION 'capital activation cannot be reversed or reassigned'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER capital_activation_monotonic BEFORE UPDATE OR DELETE ON capital_activation
    FOR EACH ROW EXECUTE FUNCTION keep_capital_activation_monotonic();

CREATE FUNCTION require_funded_order_after_activation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE armed boolean;
BEGIN
    INSERT INTO capital_activation(tenant_id) VALUES(NEW.tenant_id) ON CONFLICT DO NOTHING;
    SELECT enabled INTO armed FROM capital_activation WHERE tenant_id=NEW.tenant_id FOR SHARE;
    IF armed AND NOT EXISTS (
        SELECT 1 FROM capital_members m
        WHERE m.tenant_id=NEW.tenant_id AND m.order_id=NEW.order_id
    ) THEN
        RAISE EXCEPTION 'capital activation refuses an unfunded order'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER funded_order_activation BEFORE INSERT ON orders
    FOR EACH ROW EXECUTE FUNCTION require_funded_order_after_activation();
