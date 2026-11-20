-- An authority operator binds the approved index and benchmark signer to the
-- same reporting scope as the Accounting close issuer.
CREATE TABLE return_basis_benchmark_authorities (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    benchmark_id TEXT NOT NULL,
    issuer_id TEXT NOT NULL,
    public_key BYTEA NOT NULL CHECK (octet_length(public_key)=32),
    PRIMARY KEY (tenant_id,portfolio_id,currency),
    FOREIGN KEY (tenant_id,portfolio_id,currency) REFERENCES return_basis_authorities
);

ALTER TABLE return_basis_benchmark_authorities ENABLE ROW LEVEL SECURITY;
ALTER TABLE return_basis_benchmark_authorities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON return_basis_benchmark_authorities
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

CREATE TRIGGER return_basis_benchmark_authority_immutable BEFORE UPDATE OR DELETE ON return_basis_benchmark_authorities
    FOR EACH ROW EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_basis_benchmark_authority_no_truncate BEFORE TRUNCATE ON return_basis_benchmark_authorities
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_return_basis_mutation();
