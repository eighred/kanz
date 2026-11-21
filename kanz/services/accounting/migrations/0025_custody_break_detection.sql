-- A break can outlive several comparisons. Only the comparison that wrote
-- its current financial figures can supply their source.
CREATE TABLE custody_break_detections (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    break_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, break_id),
    FOREIGN KEY (tenant_id, break_id) REFERENCES custody_breaks (tenant_id, break_id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, run_id) REFERENCES custody_runs (tenant_id, run_id) DEFERRABLE INITIALLY DEFERRED
);
ALTER TABLE custody_break_detections ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_break_detections FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_detection_tenant ON custody_break_detections
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

CREATE OR REPLACE FUNCTION record_custody_detection() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    detection TEXT := NULLIF(current_setting('app.custody_detection_run', true), '');
BEGIN
    IF detection IS NULL THEN
        DELETE FROM custody_break_detections WHERE tenant_id = NEW.tenant_id AND break_id = NEW.break_id;
    ELSE
        INSERT INTO custody_break_detections (tenant_id, break_id, run_id)
            VALUES (NEW.tenant_id, NEW.break_id, detection)
            ON CONFLICT (tenant_id, break_id) DO UPDATE SET run_id = EXCLUDED.run_id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_break_detection
    AFTER INSERT OR UPDATE OF ibor, custodian, difference, last_seen_at ON custody_breaks
    FOR EACH ROW EXECUTE FUNCTION record_custody_detection();
