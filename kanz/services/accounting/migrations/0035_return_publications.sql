CREATE TABLE return_basis_publications (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    close_date DATE NOT NULL,
    as_of TIMESTAMPTZ NOT NULL,
    observed_payload BYTEA CHECK (octet_length(observed_payload) BETWEEN 1 AND 16384),
    PRIMARY KEY (tenant_id,portfolio_id,currency,close_date,as_of),
    FOREIGN KEY (tenant_id,portfolio_id,currency,close_date,as_of) REFERENCES return_basis_receipts
);
ALTER TABLE return_basis_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE return_basis_publications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON return_basis_publications
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

CREATE FUNCTION public.confirm_return_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.observed_payload IS NOT NULL OR NEW.observed_payload IS NULL OR
        (OLD.tenant_id, OLD.portfolio_id, OLD.currency, OLD.close_date, OLD.as_of) IS DISTINCT FROM
        (NEW.tenant_id, NEW.portfolio_id, NEW.currency, NEW.close_date, NEW.as_of) THEN
        RAISE EXCEPTION 'a return observation can only be recorded once';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER return_observation_once BEFORE UPDATE ON return_basis_publications
    FOR EACH ROW EXECUTE FUNCTION public.confirm_return_observation();
CREATE TRIGGER return_publication_no_delete BEFORE DELETE ON return_basis_publications
    FOR EACH ROW EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_publication_no_truncate BEFORE TRUNCATE ON return_basis_publications
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_return_basis_mutation();
