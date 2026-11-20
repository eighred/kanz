-- A break can outlive several comparisons. Only the comparison that wrote
-- its current financial figures can supply their source.
ALTER TABLE custody_breaks ADD COLUMN detection_run_id TEXT;
ALTER TABLE custody_breaks ADD CONSTRAINT custody_break_detection_run_fk
    FOREIGN KEY (tenant_id, detection_run_id) REFERENCES custody_runs (tenant_id, run_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE OR REPLACE FUNCTION record_custody_detection() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.detection_run_id := NULLIF(current_setting('app.custody_detection_run', true), '');
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_break_detection
    BEFORE INSERT OR UPDATE OF ibor, custodian, difference, last_seen_at ON custody_breaks
    FOR EACH ROW EXECUTE FUNCTION record_custody_detection();
