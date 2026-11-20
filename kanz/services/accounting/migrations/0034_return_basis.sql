-- Only a migration/authority operator provisions issuer keys. Data-plane
-- roles may read them. An empty authority table admits no close evidence.
CREATE TABLE return_basis_authorities (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    issuer_id TEXT NOT NULL,
    public_key BYTEA NOT NULL CHECK (octet_length(public_key)=32),
    flow_scope TEXT NOT NULL,
    PRIMARY KEY (tenant_id,portfolio_id,currency)
);

CREATE TABLE return_basis_receipts (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    currency TEXT NOT NULL,
    close_date DATE NOT NULL,
    as_of TIMESTAMPTZ NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    signed_payload BYTEA NOT NULL CHECK (octet_length(signed_payload) BETWEEN 1 AND 16384),
    signature BYTEA NOT NULL CHECK (octet_length(signature)=64),
    PRIMARY KEY (tenant_id,portfolio_id,currency,close_date,as_of),
    FOREIGN KEY (tenant_id,portfolio_id,currency) REFERENCES return_basis_authorities
);

ALTER TABLE return_basis_authorities ENABLE ROW LEVEL SECURITY;
ALTER TABLE return_basis_authorities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON return_basis_authorities
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

ALTER TABLE return_basis_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE return_basis_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON return_basis_receipts
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

CREATE FUNCTION public.refuse_return_basis_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'accepted return evidence and authority are immutable'; END $$;

CREATE TRIGGER return_basis_authority_immutable BEFORE UPDATE OR DELETE ON return_basis_authorities
    FOR EACH ROW EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_basis_receipt_immutable BEFORE UPDATE OR DELETE ON return_basis_receipts
    FOR EACH ROW EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_basis_authority_no_truncate BEFORE TRUNCATE ON return_basis_authorities
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_return_basis_mutation();
CREATE TRIGGER return_basis_receipt_no_truncate BEFORE TRUNCATE ON return_basis_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_return_basis_mutation();
