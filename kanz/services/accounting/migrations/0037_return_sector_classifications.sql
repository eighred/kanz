-- A reporting reference-data steward enrolls complete, fixed-taxonomy
-- snapshots for one UTC day. Report sources and readers have no write grant.
CREATE TABLE return_basis_sector_classifications (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    close_date DATE NOT NULL,
    known_at TIMESTAMPTZ NOT NULL CHECK (isfinite(known_at)),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(recorded_at)),
    reference TEXT NOT NULL,
    taxonomy TEXT NOT NULL,
    sectors JSONB NOT NULL CHECK (jsonb_typeof(sectors)='object' AND octet_length(sectors::text) <= 8192),
    PRIMARY KEY (tenant_id,portfolio_id,currency,close_date,known_at),
    UNIQUE (tenant_id,portfolio_id,currency,close_date,reference),
    FOREIGN KEY (tenant_id,portfolio_id,currency) REFERENCES return_basis_authorities
);

ALTER TABLE return_basis_sector_classifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE return_basis_sector_classifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON return_basis_sector_classifications
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

CREATE TRIGGER return_basis_sector_classifications_immutable BEFORE UPDATE OR DELETE ON return_basis_sector_classifications
    FOR EACH ROW EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_basis_sector_classifications_no_truncate BEFORE TRUNCATE ON return_basis_sector_classifications
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_return_basis_mutation();
