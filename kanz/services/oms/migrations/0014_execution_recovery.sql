-- Recovery evidence belongs to the same tenant and transaction boundary as
-- order fills. A mapping version is immutable: later configuration cannot
-- redefine the account/portfolio against which an execution was investigated.
ALTER TABLE order_fills ADD COLUMN raw_fill_id TEXT NOT NULL DEFAULT '';
CREATE INDEX order_fills_alias_idx ON order_fills(tenant_id, raw_fill_id) WHERE raw_fill_id <> '';

-- Old binaries still claim an unscoped fill_id during a rolling deployment.
-- Serialize both formats on the original alias so neither can race the other's
-- duplicate check. An older writer must not fold a newly scoped claim again.
CREATE FUNCTION protect_execution_claim_rollout() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE alias TEXT;
BEGIN
    alias := COALESCE(NULLIF(NEW.raw_fill_id,''), NEW.fill_id);
    PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id), hashtext('execution-alias:' || alias));
    IF NEW.fill_id = alias THEN
        IF EXISTS (SELECT 1 FROM order_fills WHERE tenant_id=NEW.tenant_id
                   AND raw_fill_id=alias AND fill_id<>alias) THEN
            IF EXISTS (SELECT 1 FROM order_fills WHERE tenant_id=NEW.tenant_id
                       AND raw_fill_id=alias AND order_id=NEW.order_id) THEN
                RETURN NULL;
            END IF;
            RAISE EXCEPTION 'legacy execution writer cannot attribute an alias owned by another order';
        END IF;
    ELSIF EXISTS (SELECT 1 FROM order_fills WHERE tenant_id=NEW.tenant_id AND fill_id=alias) THEN
        RAISE EXCEPTION 'legacy execution alias requires attribution before scoped claim';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER execution_claim_rollout BEFORE INSERT ON order_fills
    FOR EACH ROW EXECUTE FUNCTION protect_execution_claim_rollout();
CREATE TABLE execution_account_mappings (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    version TEXT NOT NULL,
    venue TEXT NOT NULL,
    venue_account_id TEXT NOT NULL,
    exchange_account_id TEXT NOT NULL,
    portfolio_id TEXT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, version),
    CHECK (version <> '' AND venue <> '' AND venue_account_id <> ''
        AND exchange_account_id <> '' AND portfolio_id <> '')
);

CREATE TABLE execution_recovery_cases (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    order_id TEXT NOT NULL,
    mapping_version TEXT,
    source_cursor TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    evidence BYTEA NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('observed', 'investigating', 'corrected', 'blocked')),
    reason TEXT NOT NULL DEFAULT '',
    checkpoint INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint >= 0),
    expected_ack_count INTEGER NOT NULL DEFAULT 0 CHECK (expected_ack_count >= 0),
    version BIGINT NOT NULL DEFAULT 0,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, case_id),
    FOREIGN KEY (tenant_id, mapping_version) REFERENCES execution_account_mappings(tenant_id, version),
    CHECK (case_id <> '' AND order_id <> '' AND source_cursor <> '' AND payload_digest <> '')
);
CREATE INDEX execution_recovery_order_idx ON execution_recovery_cases(tenant_id, order_id, recorded_at);

CREATE TABLE execution_recovery_targets (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    execution_key TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    execution_digest TEXT NOT NULL,
    PRIMARY KEY (tenant_id,case_id,execution_key),
    FOREIGN KEY (tenant_id,case_id) REFERENCES execution_recovery_cases(tenant_id,case_id)
);
CREATE TABLE execution_recovery_acks (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    execution_key TEXT NOT NULL,
    book TEXT NOT NULL CHECK (book IN ('ledger','position')),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id,case_id,execution_key,book),
    FOREIGN KEY (tenant_id,case_id,execution_key) REFERENCES execution_recovery_targets(tenant_id,case_id,execution_key)
);

CREATE TABLE execution_recovery_history (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    case_id TEXT NOT NULL,
    mapping_version TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    history BYTEA NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, case_id),
    FOREIGN KEY (tenant_id, case_id) REFERENCES execution_recovery_cases(tenant_id, case_id),
    FOREIGN KEY (tenant_id, mapping_version) REFERENCES execution_account_mappings(tenant_id, version)
);

-- Retain the execution, not merely a dedup token. Venue trade references are
-- scoped by instrument too (Binance trade IDs are per symbol).
CREATE TABLE order_executions (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    order_id TEXT NOT NULL,
    fill_id TEXT NOT NULL,
    venue TEXT NOT NULL,
    venue_account_id TEXT NOT NULL,
    instrument_id TEXT NOT NULL,
    venue_execution_id TEXT NOT NULL,
    fill BYTEA NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, order_id, fill_id)
);
CREATE UNIQUE INDEX order_executions_venue_identity ON order_executions
    (tenant_id, venue, venue_account_id, instrument_id, venue_execution_id)
    WHERE venue_execution_id <> '' AND venue_account_id <> '' AND venue <> '' AND instrument_id <> '';

ALTER TABLE execution_account_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_account_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_account_mappings
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
ALTER TABLE execution_recovery_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_recovery_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_recovery_cases
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
ALTER TABLE order_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON order_executions
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
ALTER TABLE execution_recovery_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_recovery_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_recovery_history
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
ALTER TABLE execution_recovery_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_recovery_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_recovery_targets
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
ALTER TABLE execution_recovery_acks ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_recovery_acks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_recovery_acks
    USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());

CREATE FUNCTION preserve_execution_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution evidence is immutable; append a correction';
END;
$$;
CREATE TRIGGER execution_mapping_immutable BEFORE UPDATE OR DELETE ON execution_account_mappings
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER order_execution_immutable BEFORE UPDATE OR DELETE ON order_executions
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_history_immutable BEFORE UPDATE OR DELETE ON execution_recovery_history
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_target_immutable BEFORE UPDATE OR DELETE ON execution_recovery_targets
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_ack_immutable BEFORE UPDATE OR DELETE ON execution_recovery_acks
    FOR EACH ROW EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER execution_mapping_no_truncate BEFORE TRUNCATE ON execution_account_mappings FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER order_execution_no_truncate BEFORE TRUNCATE ON order_executions FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_history_no_truncate BEFORE TRUNCATE ON execution_recovery_history FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_target_no_truncate BEFORE TRUNCATE ON execution_recovery_targets FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_ack_no_truncate BEFORE TRUNCATE ON execution_recovery_acks FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();
CREATE TRIGGER recovery_case_no_truncate BEFORE TRUNCATE ON execution_recovery_cases FOR EACH STATEMENT EXECUTE FUNCTION preserve_execution_evidence();

CREATE FUNCTION preserve_recovery_case_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'recovery evidence cannot be deleted';
    END IF;
    IF ROW(NEW.tenant_id, NEW.case_id, NEW.order_id, NEW.source_cursor,
           NEW.payload_digest, NEW.evidence, NEW.recorded_at)
       IS DISTINCT FROM
       ROW(OLD.tenant_id, OLD.case_id, OLD.order_id, OLD.source_cursor,
           OLD.payload_digest, OLD.evidence, OLD.recorded_at) THEN
        RAISE EXCEPTION 'recovery source evidence is immutable';
    END IF;
    IF OLD.mapping_version IS NOT NULL AND NEW.mapping_version IS DISTINCT FROM OLD.mapping_version THEN
        RAISE EXCEPTION 'recovery mapping cannot be replaced';
    END IF;
    IF NEW.checkpoint < OLD.checkpoint OR NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'recovery checkpoint must advance under compare-and-swap';
    END IF;
    IF OLD.status = 'corrected' THEN
        RAISE EXCEPTION 'completed recovery cannot be reopened';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER recovery_case_evidence_immutable BEFORE UPDATE OR DELETE ON execution_recovery_cases
    FOR EACH ROW EXECUTE FUNCTION preserve_recovery_case_evidence();
