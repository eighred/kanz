-- AUDIT-01a/b: the append-only, tamper-evident audit log.
--
-- Every materialized event is one immutable row. seq is the chain order;
-- prev_hash/hash are the AUDIT-01b hash chain (hash = sha256(prev_hash ||
-- canonical(row))), so any rewrite of a past row breaks every later hash and is
-- caught by chain verification.
--
-- WORM at the database layer: the trigger below makes UPDATE and DELETE raise,
-- so the table is insert-only even to a compromised app role — the storage half
-- of AUDIT-01b's tamper-evidence (object-lock/WORM is the equivalent at the
-- object-store tier; see internal/chain/README.md). Genuine retention purge
-- (AUDIT-01d) is a privileged lifecycle operation outside this role, gated by
-- the retention policy + legal holds.
--
-- NOT tenant-RLS-scoped: the audit log is a cross-cutting compliance record that
-- an auditor reads ACROSS tenants; tenant_id is a column for filtering/reporting,
-- not an isolation boundary here (the same call market-data/schema-registry made
-- for universal data).
--
-- Tenant-scoped read access is therefore enforced ENTIRELY at the query API, and
-- for a long time it was not enforced at all: `GET /v1/audit/events` took its
-- tenant from a caller-supplied `?tenant=`, and `GET /v1/audit/events/{id}`
-- checked no tenant whatsoever. This line asserted the enforcement existed while
-- it did not, which is worse than silence -- it is the comment a reviewer trusts
-- instead of reading the handler. Both are now scoped to the gateway-injected
-- principal (server.tenantOf).
--
-- STILL UNSCOPED, deliberately flagged rather than quietly fixed, because each
-- needs a decision and not a patch: /v1/audit/lineage/{id}, /v1/audit/verify and
-- /v1/audit/reports/{template}. Chain verification is cross-tenant BY DESIGN --
-- the hash chain spans every record, and scoping it would break the
-- tamper-evidence it exists to provide -- so the answer there is probably an
-- operator capability, not a tenant filter. Do not assume these are covered.

CREATE TABLE IF NOT EXISTS audit_log (
    seq            BIGINT      PRIMARY KEY,
    event_id       TEXT        NOT NULL UNIQUE,
    correlation_id TEXT        NOT NULL,
    causation_id   TEXT        NOT NULL DEFAULT '',
    domain         TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    event_class    TEXT        NOT NULL,
    tenant_id      TEXT        NOT NULL DEFAULT '',
    source         TEXT        NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL,
    kind           TEXT        NOT NULL,
    summary        TEXT        NOT NULL,
    attributes     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    schema_ref     TEXT        NOT NULL DEFAULT '',
    prev_hash      TEXT        NOT NULL,
    hash           TEXT        NOT NULL
);

-- Lineage walks (AUDIT-01c) and the report filters (AUDIT-01d).
CREATE INDEX IF NOT EXISTS audit_log_correlation_idx ON audit_log (correlation_id);
CREATE INDEX IF NOT EXISTS audit_log_causation_idx   ON audit_log (causation_id);
CREATE INDEX IF NOT EXISTS audit_log_tenant_time_idx ON audit_log (tenant_id, occurred_at);
CREATE INDEX IF NOT EXISTS audit_log_kind_idx        ON audit_log (kind);

-- WORM enforcement: an audited row is immutable. UPDATE/DELETE by the app role
-- is a tamper attempt — reject it at the engine, not just by convention.
CREATE OR REPLACE FUNCTION audit_log_worm() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only (WORM): % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_log_no_mutate ON audit_log;
CREATE TRIGGER audit_log_no_mutate
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_worm();
