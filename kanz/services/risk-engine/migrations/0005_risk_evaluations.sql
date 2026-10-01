-- Complete evaluation manifests share immutable input objects. All inserts are
-- committed together, before the caller can serve or publish the calculation.
CREATE TABLE risk_input_objects (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    digest text NOT NULL CHECK (length(digest)=64),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 33554432),
    PRIMARY KEY (tenant_id,digest),
    CHECK (digest=encode(sha256(payload),'hex'))
);
CREATE TABLE risk_evaluations (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    digest text NOT NULL CHECK (length(digest)=64),
    portfolio_id text NOT NULL CHECK (portfolio_id<>''),
    as_of_ns bigint NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    manifest bytea NOT NULL CHECK (octet_length(manifest) BETWEEN 1 AND 33554432),
    exposure bytea NOT NULL CHECK (octet_length(exposure) BETWEEN 1 AND 33554432),
    measures bytea NOT NULL CHECK (octet_length(measures) BETWEEN 1 AND 33554432),
    PRIMARY KEY (tenant_id,digest)
);
CREATE INDEX risk_evaluations_history ON risk_evaluations (tenant_id,portfolio_id,as_of_ns DESC,recorded_at DESC,digest);
CREATE TABLE risk_evaluation_inputs (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    evaluation_digest text NOT NULL,
    input_digest text NOT NULL,
    PRIMARY KEY (tenant_id,evaluation_digest,input_digest),
    FOREIGN KEY (tenant_id,evaluation_digest) REFERENCES risk_evaluations(tenant_id,digest) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,input_digest) REFERENCES risk_input_objects(tenant_id,digest)
);
ALTER TABLE risk_input_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_input_objects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON risk_input_objects USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
ALTER TABLE risk_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON risk_evaluations USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
ALTER TABLE risk_evaluation_inputs ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_evaluation_inputs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON risk_evaluation_inputs USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE TRIGGER risk_inputs_immutable BEFORE UPDATE ON risk_input_objects FOR EACH ROW EXECUTE FUNCTION reject_risk_artifact_update();
CREATE TRIGGER risk_evaluations_immutable BEFORE UPDATE ON risk_evaluations FOR EACH ROW EXECUTE FUNCTION reject_risk_artifact_update();
CREATE TRIGGER risk_evaluation_inputs_immutable BEFORE UPDATE ON risk_evaluation_inputs FOR EACH ROW EXECUTE FUNCTION reject_risk_artifact_update();
