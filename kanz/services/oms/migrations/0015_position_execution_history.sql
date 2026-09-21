ALTER TABLE position_fills ADD COLUMN raw_fill_id TEXT NOT NULL DEFAULT '';
CREATE INDEX position_fills_alias_idx ON position_fills(tenant_id, raw_fill_id) WHERE raw_fill_id <> '';

CREATE TABLE position_execution_history (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    execution_key TEXT NOT NULL,
    raw_fill_id TEXT NOT NULL,
    portfolio_id TEXT NOT NULL,
    venue TEXT NOT NULL,
    instrument_id TEXT NOT NULL,
    execution_time TIMESTAMPTZ NOT NULL,
    time_proven BOOLEAN NOT NULL,
    fill BYTEA NOT NULL,
    PRIMARY KEY (tenant_id, execution_key)
);
CREATE INDEX position_execution_time_idx ON position_execution_history
    (tenant_id, portfolio_id, venue, instrument_id, execution_time, execution_key);

-- Existing holdings cannot be reconstructed from a quantity/average alone.
-- Record that limitation once; never replace an unknown historical prefix with
-- a zero baseline merely because the current position happens to be flat.
CREATE TABLE position_execution_basis (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    venue TEXT NOT NULL,
    instrument_id TEXT NOT NULL,
    complete BOOLEAN NOT NULL,
    last_execution_time TIMESTAMPTZ NOT NULL,
    last_execution_key TEXT NOT NULL,
    PRIMARY KEY (tenant_id, portfolio_id, venue, instrument_id)
);

ALTER TABLE position_execution_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE position_execution_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON position_execution_history
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
ALTER TABLE position_execution_basis ENABLE ROW LEVEL SECURITY;
ALTER TABLE position_execution_basis FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON position_execution_basis
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE TRIGGER position_execution_immutable BEFORE UPDATE OR DELETE ON position_execution_history
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER position_execution_no_truncate BEFORE TRUNCATE ON position_execution_history
    FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();

CREATE FUNCTION protect_position_claim_rollout() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE alias TEXT;
BEGIN
    alias := COALESCE(NULLIF(NEW.raw_fill_id,''), NEW.fill_id);
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext('position-execution-alias:' || alias));
    IF NEW.fill_id=alias THEN
        IF EXISTS (SELECT 1 FROM position_fills WHERE tenant_id=NEW.tenant_id
                   AND raw_fill_id=alias AND fill_id<>alias) THEN
            RAISE EXCEPTION 'legacy position writer cannot prove ownership of a scoped execution alias';
        END IF;
    ELSIF EXISTS (SELECT 1 FROM position_fills WHERE tenant_id=NEW.tenant_id AND fill_id=alias) THEN
        RAISE EXCEPTION 'legacy position claim requires attribution before scoped claim';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER position_claim_rollout BEFORE INSERT ON position_fills
    FOR EACH ROW EXECUTE FUNCTION protect_position_claim_rollout();
