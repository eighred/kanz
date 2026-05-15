-- 0001: initial schema-registry storage.
--
-- One row per (schema_id, version). The pair is immutable once published —
-- see kanz-schemas/docs/schema-evolution.md §6.
CREATE TABLE schemas (
    schema_id   TEXT        NOT NULL,
    version     BIGINT      NOT NULL CHECK (version > 0),
    descriptor  BYTEA       NOT NULL,
    fingerprint TEXT        NOT NULL,
    source_tag  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schema_id, version)
);
