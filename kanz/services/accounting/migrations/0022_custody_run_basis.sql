-- NULL preserves the unknown basis of historical runs, skipped comparisons and older writers.
ALTER TABLE custody_runs ADD COLUMN book_basis JSONB;
COMMENT ON COLUMN custody_runs.book_basis IS
    'Observed scope mode and sorted account IDs by custodian for this portfolio. '
    'journal_position bounds the WORM portfolio prefix before canonical revision selection and UTC business-day close. '
    'Pre-index entries belong to the immutable legacy prefix. NULL denotes unavailable evidence.';
ALTER TABLE custody_breaks ADD COLUMN comparison_scope JSONB;

-- Only a detection writes the financial figures. An operator assignment or a
-- resolve-on-absence sweep cannot replace the scope that first saw the break.
-- A legacy writer contributes an unknown scope even during a rolling deployment.
CREATE OR REPLACE FUNCTION record_custody_break_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.comparison_scope := NULLIF(current_setting('app.custody_break_scope', true), '')::jsonb;
    IF TG_OP = 'UPDATE' AND NEW.comparison_scope IS DISTINCT FROM OLD.comparison_scope THEN
        NEW.first_seen_at := NEW.last_seen_at;
        NEW.status_changed_at := NEW.last_seen_at;
        NEW.status := 'open';
        NEW.assignee := '';
        NEW.explanation := '';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_break_scope_on_detection
    BEFORE INSERT OR UPDATE OF ibor, custodian, difference, last_seen_at ON custody_breaks
    FOR EACH ROW EXECUTE FUNCTION record_custody_break_scope();
