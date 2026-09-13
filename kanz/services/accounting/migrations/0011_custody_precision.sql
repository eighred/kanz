-- Pre-upgrade rows passed through an eight-place renderer. Their original
-- precision cannot be recovered from those strings. Only source replay and a
-- fresh reconciliation may establish verified values.
ALTER TABLE custody_statements ADD COLUMN values_verified BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE custody_breaks ADD COLUMN values_verified BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE custody_runs ADD COLUMN values_verified BOOLEAN NOT NULL DEFAULT false;

-- Old binaries do not set this transaction-local writer contract. If one
-- rewrites figures during a rolling upgrade, it must invalidate verification.
CREATE OR REPLACE FUNCTION custody_verify_writer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.values_verified := NEW.values_verified AND
        COALESCE(current_setting('app.custody_exact_writer', true), '') = 'v1';
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_statement_precision BEFORE INSERT OR UPDATE OF positions, cash, transactions
    ON custody_statements FOR EACH ROW EXECUTE FUNCTION custody_verify_writer();
CREATE TRIGGER custody_break_precision BEFORE INSERT OR UPDATE OF ibor, custodian, difference
    ON custody_breaks FOR EACH ROW EXECUTE FUNCTION custody_verify_writer();
CREATE TRIGGER custody_run_precision BEFORE INSERT OR UPDATE OF tolerance
    ON custody_runs FOR EACH ROW EXECUTE FUNCTION custody_verify_writer();
