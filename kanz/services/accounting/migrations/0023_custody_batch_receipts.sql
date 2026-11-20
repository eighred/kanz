-- The signed bytes and the outbound statement enter the same transaction. The
-- reconciler's statement_id identifies the specific receipt across corrections.
CREATE TABLE custody_batch_receipts (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    portfolio_id TEXT NOT NULL,
    custodian_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    business_date DATE NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    previous_sha256 TEXT NOT NULL,
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    signature BYTEA NOT NULL CHECK (octet_length(signature) = 64),
    signed_batch BYTEA NOT NULL CHECK (octet_length(signed_batch) BETWEEN 1 AND 1048576),
    received_at TIMESTAMPTZ NOT NULL,
    statement_id TEXT NOT NULL,
    wire_sha256 BYTEA NOT NULL CHECK (octet_length(wire_sha256) = 32),
    PRIMARY KEY (tenant_id,portfolio_id,custodian_id,business_date,revision),
    UNIQUE (tenant_id,statement_id),
    CHECK ((revision = 1 AND previous_sha256 = '') OR
           (revision > 1 AND previous_sha256 ~ '^[0-9a-f]{64}$'))
);
ALTER TABLE custody_batch_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_batch_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_batch_tenant ON custody_batch_receipts
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
CREATE TRIGGER custody_batch_no_mutate BEFORE UPDATE OR DELETE ON custody_batch_receipts
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_batch_no_truncate BEFORE TRUNCATE ON custody_batch_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm();
