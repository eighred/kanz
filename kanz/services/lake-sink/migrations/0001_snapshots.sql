-- Optional finite snapshot owner. Apply to the lake service's own schema using
-- the migration owner; runtime uses a NOSUPERUSER/NOBYPASSRLS tenant pool.
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $$
DECLARE t text;
BEGIN
    t := current_setting('app.tenant_id', true);
    IF t IS NULL OR t = '' THEN
        RAISE EXCEPTION 'tenant scope missing' USING ERRCODE = '42501';
    END IF;
    RETURN t;
END; $$;

CREATE TABLE lake_snapshots (
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    snapshot_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    request_wire BYTEA NOT NULL CHECK (octet_length(request_wire) <= 65536),
    request_sha256 BYTEA NOT NULL CHECK (octet_length(request_sha256) = 32),
    requested_by BYTEA NOT NULL CHECK (octet_length(requested_by) <= 16384),
    state INTEGER NOT NULL CHECK (state IN (1,2,3,4)),
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation BETWEEN 0 AND 3),
    source_scope INTEGER NOT NULL CHECK (source_scope = 1),
    cut BYTEA CHECK (octet_length(cut) <= 65536),
    manifest BYTEA CHECK (octet_length(manifest) <= 65536),
    manifest_sha256 BYTEA CHECK (octet_length(manifest_sha256) = 32),
    parquet_sha256 BYTEA CHECK (octet_length(parquet_sha256) = 32),
    output_name TEXT,
    row_count BIGINT NOT NULL DEFAULT 0 CHECK (row_count BETWEEN 0 AND 100000),
    parquet_bytes BIGINT NOT NULL DEFAULT 0 CHECK (parquet_bytes BETWEEN 0 AND 100663296),
    failure INTEGER NOT NULL DEFAULT 0 CHECK (failure BETWEEN 0 AND 5),
    created_at TIMESTAMPTZ NOT NULL,
    sealed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, snapshot_id),
    UNIQUE (tenant_id, request_id),
    CHECK (state <> 3 OR (cut IS NOT NULL AND manifest IS NOT NULL AND manifest_sha256 IS NOT NULL
        AND parquet_sha256 IS NOT NULL AND output_name IS NOT NULL AND parquet_bytes > 0 AND sealed_at IS NOT NULL AND failure = 0)),
    CHECK (state <> 4 OR failure <> 0)
);
ALTER TABLE lake_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE lake_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lake_snapshots
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- A retry may advance its generation, but cannot silently reselect a growing
-- landing prefix or rewrite the authenticated acceptance decision.
CREATE FUNCTION lake_snapshot_preserve_request_cut() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.snapshot_id IS DISTINCT FROM OLD.snapshot_id
       OR NEW.request_id IS DISTINCT FROM OLD.request_id OR NEW.request_wire IS DISTINCT FROM OLD.request_wire
       OR NEW.request_sha256 IS DISTINCT FROM OLD.request_sha256 OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.source_scope IS DISTINCT FROM OLD.source_scope
       OR (OLD.cut IS NOT NULL AND NEW.cut IS DISTINCT FROM OLD.cut)
       OR (OLD.state IN (3,4) AND NEW IS DISTINCT FROM OLD) THEN
        RAISE EXCEPTION 'snapshot accepted custody is immutable';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER lake_snapshot_immutable BEFORE UPDATE ON lake_snapshots
    FOR EACH ROW EXECUTE FUNCTION lake_snapshot_preserve_request_cut();

-- Exact canonical internal/outbox storage shape; ownership is explicit from the
-- first write rather than inferred from a subject or a migration default.
CREATE TABLE outbox (
    id BIGSERIAL NOT NULL,
    tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
    producer_service TEXT NOT NULL CHECK (producer_service <> '' AND producer_service = btrim(producer_service)),
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
    PRIMARY KEY (tenant_id, id)
);
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
CREATE INDEX outbox_service_pending_idx ON outbox (tenant_id, producer_service, partition_key, id) WHERE published_at IS NULL;
CREATE FUNCTION outbox_preserve_producer_service() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.producer_service IS DISTINCT FROM OLD.producer_service THEN
        RAISE EXCEPTION 'outbox producer service is immutable';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER outbox_producer_service_immutable BEFORE UPDATE OF producer_service ON outbox
    FOR EACH ROW EXECUTE FUNCTION outbox_preserve_producer_service();
