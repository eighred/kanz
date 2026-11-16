-- 0001: initial schema-registry storage.
--
-- One row per (schema_id, version). The pair is immutable once published —
-- see kanz-schemas/README.md § Schema Evolution §6.
CREATE TABLE schemas (
    schema_id   TEXT        NOT NULL,
    version     BIGINT      NOT NULL CHECK (version > 0),
    descriptor  BYTEA       NOT NULL,
    fingerprint TEXT        NOT NULL,
    source_tag  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schema_id, version)
);

-- MT-01d note: the schema registry is INTENTIONALLY NOT tenant-partitioned.
-- Schemas are universal platform contracts (like the envelope) — every tenant's
-- producers and consumers resolve the same `payload_schema_ref`, so partitioning
-- them by tenant would fork the contract and break cross-tenant interop. There
-- is no per-tenant schema data to isolate, so RLS is omitted here by design (it
-- applies to genuinely tenant-owned state — risk-engine, 0002_tenant_rls.sql).
