-- A filing, its original signature, a separate context-chain witness and every
-- publication are committed together. Existing audit_chain_links stays the one
-- global chain; the new artifact/outbox records are tenant scoped.
CREATE FUNCTION app_current_tenant() RETURNS TEXT LANGUAGE plpgsql STABLE AS $$
DECLARE tenant TEXT := current_setting('app.tenant_id', true);
BEGIN
    IF tenant IS NULL OR tenant = '' THEN
        RAISE EXCEPTION 'tenant scope required';
    END IF;
    RETURN tenant;
END;
$$;

CREATE TABLE regulatory_filing_artifacts (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    semantic_key TEXT NOT NULL CHECK (semantic_key ~ '^[0-9a-f]{64}$'),
    report_id UUID NOT NULL UNIQUE,
    portfolio_id TEXT NOT NULL,
    framework TEXT NOT NULL,
    as_of TIMESTAMPTZ NOT NULL,
    input_digest TEXT NOT NULL CHECK (input_digest ~ '^[0-9a-f]{64}$'),
    binding_hash TEXT NOT NULL UNIQUE REFERENCES audit_chain_links(hash),
    artifact BYTEA NOT NULL,
    response BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, semantic_key)
);

-- UUIDs are distinct across report, ESG, carbon and aggregate publications,
-- including concurrent requests. They are domain identities, retained on retry.
CREATE TABLE regulatory_filing_publications (
    publication_id UUID PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    semantic_key TEXT NOT NULL,
    FOREIGN KEY (tenant_id, semantic_key)
        REFERENCES regulatory_filing_artifacts(tenant_id, semantic_key)
);

ALTER TABLE regulatory_filing_artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_filing_artifacts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON regulatory_filing_artifacts
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
ALTER TABLE regulatory_filing_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_filing_publications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON regulatory_filing_publications
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

CREATE FUNCTION regulatory_filing_immutable() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'filing artifacts and publication identities are append-only';
END;
$$;
CREATE TRIGGER regulatory_filing_artifacts_immutable
    BEFORE UPDATE OR DELETE OR TRUNCATE ON regulatory_filing_artifacts
    FOR EACH STATEMENT EXECUTE FUNCTION regulatory_filing_immutable();
CREATE TRIGGER regulatory_filing_publications_immutable
    BEFORE UPDATE OR DELETE OR TRUNCATE ON regulatory_filing_publications
    FOR EACH STATEMENT EXECUTE FUNCTION regulatory_filing_immutable();

-- Canonical outbox storage/relay schema. The global chain lock serializes each
-- artifact transaction through commit, so enqueue order is commit order even
-- when two different filing requests share one portfolio partition key.
CREATE TABLE outbox (
    id BIGSERIAL NOT NULL,
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    producer_service TEXT NOT NULL CHECK (producer_service = 'regulatory'),
    envelope_tenant_id TEXT NOT NULL,
    partition_key TEXT NOT NULL,
    subject TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_class INTEGER NOT NULL,
    schema_version INTEGER NOT NULL,
    domain TEXT NOT NULL,
    payload_schema_ref TEXT NOT NULL,
    event_time TIMESTAMPTZ NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    causation_id TEXT NOT NULL DEFAULT '',
    trace_context TEXT NOT NULL DEFAULT '',
    payload BYTEA NOT NULL,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id),
    CHECK (envelope_tenant_id = tenant_id)
);
CREATE INDEX outbox_pending_idx ON outbox (tenant_id, partition_key, id)
    WHERE published_at IS NULL;
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
