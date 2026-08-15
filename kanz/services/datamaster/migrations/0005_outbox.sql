-- #410 — the override FACT reaches the estate through a transactional outbox.
--
-- WHY AN OUTBOX AND NOT A PUBLISH.
--
-- The override is a named human's signed decision, written to an append-only
-- audit trail. services/audit projects bus FACTs into a signed, tamper-evident
-- store and cannot see this service's tables, so until the FACT exists the
-- approval is durable HERE and absent from the platform's audit trail.
--
-- Publishing directly at approval time forces a choice between two wrong
-- answers, and this table exists to refuse both:
--
--   * fail the override when the broker is down -- a valuation outage caused by
--     an audit dependency, on the path an operator uses to unblock valuation;
--   * publish best-effort and drop it -- an audit gap that nothing reports,
--     which is precisely the silence #410 exists to end.
--
-- Writing the override row and this row in ONE transaction has neither failure
-- mode: the override commits or it does not, and the FACT follows when the
-- broker returns.
--
-- The shape is deliberately IDENTICAL to services/oms/migrations/0006_outbox.sql
-- because internal/outbox reads both. The relay is the same code, the columns
-- are its contract, and a column that differed here would be a second dialect of
-- one table.
CREATE TABLE outbox (
    id                 BIGSERIAL   NOT NULL,
    tenant_id          TEXT        NOT NULL DEFAULT app_current_tenant(),  -- storage scope; RLS filters on this
    envelope_tenant_id TEXT        NOT NULL,   -- published tenant; NOT the same thing
    partition_key      TEXT        NOT NULL,   -- exception_id; the unit order is preserved within
    subject            TEXT        NOT NULL,
    event_type         TEXT        NOT NULL,
    event_class        INTEGER     NOT NULL,   -- envelope.v1.EventClass
    schema_version     INTEGER     NOT NULL,
    domain             TEXT        NOT NULL,
    payload_schema_ref TEXT        NOT NULL,   -- "{proto full name}:{version}"; the relay's ONLY handle on the payload type
    event_time         TIMESTAMPTZ NOT NULL,   -- when it HAPPENED, not when it is sent
    correlation_id     TEXT        NOT NULL DEFAULT '',
    causation_id       TEXT        NOT NULL DEFAULT '',
    trace_context      TEXT        NOT NULL DEFAULT '',
    payload            BYTEA       NOT NULL,
    enqueued_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,            -- NULL => the estate has not heard this yet
    attempts           INTEGER     NOT NULL DEFAULT 0,
    last_error         TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- The relay's only read: unpublished records, by key, in id order. PARTIAL on
-- published_at IS NULL so the index holds the BACKLOG and not the history.
CREATE INDEX outbox_pending_idx ON outbox (tenant_id, partition_key, id)
    WHERE published_at IS NULL;

-- Tenant isolation (MT-01d/e) on the STORAGE tenant, load-bearing here in a way
-- it is not on the other tables in this service: this one holds the FACTS
-- THEMSELVES, envelope and payload. A relay that could read another storage
-- tenant's rows would not merely read them -- it would PUBLISH them onto the
-- shared bus.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
