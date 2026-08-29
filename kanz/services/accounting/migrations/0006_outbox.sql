-- 0006: the transactional outbox for the cash announcement (#804, #292).
--
-- THE LEDGER COMMIT AND ITS ANNOUNCEMENT WERE TWO INDEPENDENT WRITES.
--
-- consume.Fold called store.Append and then announce, and the announce error was
-- deliberately discarded — counter plus an ERROR log. That trade-off was right
-- and is unchanged: the ledger is the book of record, the announcement is
-- DERIVED, and nacking the fold to retry a publish would turn a broker blip into
-- a stalled ledger. A consumer's staleness bound makes a lost announcement safe,
-- because it ages the balance out to UNKNOWN and the buying-power rule fails
-- closed on that.
--
-- What the trade-off did not state was the RECOVERY, and there wasn't one. The
-- only thing that re-announced a portfolio was THE NEXT FOLD FOR THAT PORTFOLIO
-- — the composition root says so in as many words, "every announcement is caused
-- by a fold". No ticker, no compensator, no outbox. So one broker blip during
-- one portfolio's fold refused EVERY order for that portfolio under a
-- buying-power mandate, indefinitely, until unrelated activity happened to
-- arrive. For a portfolio that trades a few times a day that is a trading outage
-- measured in hours, produced by a transient the platform recovered from in
-- seconds. Fail-closed is the right direction and was the wrong duration.
--
-- A row here is written IN THE SAME TRANSACTION as the journal entry it
-- announces. Either both land or neither does, and the relay retries the publish
-- on its own schedule. The failure mode moves from "the announcement is lost and
-- only the next trade will fix it" to "the announcement is late and a queue
-- depth says so".
--
-- # This table is the OMS's, deliberately unchanged in shape
--
-- Same columns, same partial index, same two-tenant split, drained by the same
-- internal/outbox relay. The generalisation was written for a second adopter and
-- this is the third; a variant schema here would fork the relay's reads for no
-- reason. The comments that differ are the ones about THIS service, below.
--
-- # Ordering, and what supplies it here
--
-- id is a BIGSERIAL and id order is the order the relay publishes in. A sequence
-- allocates at INSERT time, not COMMIT time, so id order and commit order can
-- disagree in general: transaction A takes id 5, B takes 6 and commits first, and
-- a relay reading between the two commits publishes 6 and leaves 5 behind it
-- forever.
--
-- IN THE OMS THAT CANNOT HAPPEN BECAUSE EVERY ENQUEUE RIDES A VERSIONED WRITE —
-- admission is one winner per order_id and every transition after it carries a
-- CAS on orders.version, so two transactions for one key cannot both commit.
-- THE LEDGER HAS NO SUCH WRITE. It is append-only with ON CONFLICT DO NOTHING,
-- and two folds for one portfolio commit independently.
--
-- So ledger.Postgres.Append takes a per-(tenant, portfolio) ADVISORY TRANSACTION
-- LOCK before it writes, which is what makes id order commit order for this
-- table's partition key. That lock is not an optimisation and must not be
-- removed: without it two folds for one portfolio interleave, the relay publishes
-- the older balance last, and internal/cashview replaces unconditionally with no
-- as_of guard — so the stale level is what every buying-power check then reads.
-- It is the same argument, and the same lock shape, as
-- services/oms/internal/position's lockInstrument (#795).
--
-- # The partition key is the portfolio
--
-- Not the entry id. The FACT is a LEVEL for one portfolio, and the granularity a
-- consumer folds at is the portfolio — cashview keys its map by portfolio_id.
-- Keying on the entry would order nothing that matters and would make every fold
-- its own key.
--
-- # The two tenants
--
-- tenant_id is the STORAGE scope, defaulted from the connection exactly as
-- ledger_entries.tenant_id is, so the record lives in the same isolation domain
-- as the entry it announces. envelope_tenant_id is what goes on the WIRE. They
-- are separate columns for the reason services/oms/migrations/0006 gives at
-- length: writing the envelope's tenant into tenant_id would put the row outside
-- the connection's RLS scope and the WITH CHECK would refuse it.
CREATE TABLE outbox (
    id                 BIGSERIAL   NOT NULL,
    tenant_id          TEXT        NOT NULL DEFAULT app_current_tenant(),  -- storage scope; RLS filters on this
    envelope_tenant_id TEXT        NOT NULL,   -- published tenant; NOT the same thing
    partition_key      TEXT        NOT NULL,   -- portfolio_id; the unit order is preserved within
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
    payload            BYTEA       NOT NULL,   -- marshaled accounting.v1.PortfolioCashBalance (exact decimals)
    enqueued_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,            -- NULL ⇒ the estate has not heard this yet
    attempts           INTEGER     NOT NULL DEFAULT 0,
    last_error         TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- The relay's only read: unpublished records, by key, in id order. PARTIAL on
-- published_at IS NULL so the index holds the BACKLOG and not the history.
CREATE INDEX outbox_pending_idx ON outbox (tenant_id, partition_key, id)
    WHERE published_at IS NULL;

-- Tenant isolation (MT-01d/e) on the STORAGE tenant. It is load-bearing in a way
-- it is not on ledger_entries: this table holds the FACTS THEMSELVES, envelope
-- and payload, and a relay that could read another storage tenant's rows would
-- not merely read them, it would PUBLISH them onto the shared bus. That is why
-- the relay runs in-process on this service's own tenant-scoped pool.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
