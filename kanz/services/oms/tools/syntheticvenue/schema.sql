-- Installed only in the disposable venue database by the synthetic slice harness.
CREATE SCHEMA synthetic_venue;
CREATE TABLE synthetic_venue.receipts (
    tenant_id text NOT NULL,
    portfolio_id text NOT NULL,
    order_id text NOT NULL,
    terms bytea NOT NULL,
    execution bytea,
    working_partial boolean NOT NULL DEFAULT false CHECK (NOT working_partial OR execution IS NOT NULL),
    remainder_execution bytea CHECK (remainder_execution IS NULL OR
        (execution IS NOT NULL AND NOT working_partial)),
    PRIMARY KEY (tenant_id, portfolio_id, order_id)
);
ALTER TABLE synthetic_venue.receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE synthetic_venue.receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY fixture_tenant ON synthetic_venue.receipts
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
REVOKE ALL ON SCHEMA synthetic_venue FROM PUBLIC;
GRANT USAGE ON SCHEMA synthetic_venue TO kanz_synthetic_venue;
-- Once a fill is recorded, cancellation can close the order but cannot
-- replace the execution. The second receipt is possible only from a working half.
CREATE FUNCTION synthetic_venue.guard_executions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.remainder_execution IS NOT NULL THEN
            RAISE EXCEPTION 'remainder requires a working partial receipt';
        END IF;
    ELSIF (OLD.execution IS NOT NULL AND NEW.execution IS DISTINCT FROM OLD.execution)
       OR (OLD.remainder_execution IS NOT NULL AND NEW.remainder_execution IS DISTINCT FROM OLD.remainder_execution)
       OR (NEW.remainder_execution IS DISTINCT FROM OLD.remainder_execution AND NOT OLD.working_partial)
       OR (NEW.working_partial AND NOT OLD.working_partial) THEN
        RAISE EXCEPTION 'invalid synthetic execution receipt transition';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_executions BEFORE INSERT OR UPDATE ON synthetic_venue.receipts
    FOR EACH ROW EXECUTE FUNCTION synthetic_venue.guard_executions();
GRANT SELECT, INSERT, UPDATE (execution, working_partial, remainder_execution)
    ON synthetic_venue.receipts TO kanz_synthetic_venue;
