-- Immutable pricing artifacts. Nanoseconds preserve the protobuf identity;
-- timestamptz alone would round distinct calibration timestamps together.
CREATE TABLE risk_model_artifacts (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    kind text NOT NULL CHECK (kind IN ('curve', 'factor')),
    artifact_key text NOT NULL CHECK (artifact_key <> ''),
    as_of_ns bigint NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 8388608),
    digest bytea NOT NULL CHECK (octet_length(digest) = 32),
    PRIMARY KEY (tenant_id, kind, artifact_key, as_of_ns)
);
ALTER TABLE risk_model_artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_model_artifacts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON risk_model_artifacts
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

CREATE OR REPLACE FUNCTION reject_risk_artifact_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'risk artifact identity is immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER risk_artifacts_immutable BEFORE UPDATE ON risk_model_artifacts
    FOR EACH ROW EXECUTE FUNCTION reject_risk_artifact_update();
