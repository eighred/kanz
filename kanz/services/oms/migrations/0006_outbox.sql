-- 0006: the transactional outbox (#292).
--
-- EVERY STATE CHANGE IN THIS SERVICE WAS TWO INDEPENDENT WRITES.
--
-- A store write, then a publish. Between them the process can die, the broker
-- can refuse, the network can drop — and the two halves disagree with nothing to
-- notice. The answer, three separate times, was a hand-rolled compensator: a
-- marker column inside the state proto plus a recovery branch that republishes
-- (cancel_announced_at, outcome_announced_at, accepted_announced_at — fields 18,
-- 19 and 20 of order/v1/order_events.proto, and #238's periodic sweep). Each was
-- correct. None of them generalized: a new FACT on a transition needs a new
-- marker, a new recovery branch and a new test, and nothing forced that — which
-- is exactly how accepted_announced_at came to be missing while its two siblings
-- existed.
--
-- A row here is written IN THE SAME TRANSACTION as the state change it
-- announces. Either both land or neither does. A relay drains the table and
-- publishes. The failure mode moves from "a FACT is lost and a compensator has
-- to notice" to "a FACT is late and a queue depth says so" — and the second one
-- is visible.
--
-- # Why the payload is opaque bytes
--
-- Same stance as orders.state and the risk-engine's PERS-01 blob: the payload is
-- a marshaled protobuf carrying exact base-10 Decimal, and `double` is banned on
-- any path that moves capital. There is no lossless native column. The
-- ENVELOPE-shaping fields beside it are text and timestamps because the relay
-- must read them without decoding the payload, and because an operator asking
-- "what has not gone out?" should not need a protobuf decoder to answer it.
--
-- # Why the lineage columns exist
--
-- correlation_id, causation_id and trace_context are normally inherited from the
-- handler's context — bus.Consumer stashes them off the inbound envelope so a
-- derived FACT is automatically linked to the command that caused it. The relay
-- publishes from a ticker, long after that context is gone. Unstored, they would
-- simply be absent: the FACTs would publish and validate fine and quietly stop
-- being traceable to the order that produced them. That is the kind of loss
-- nothing fails on, so it is written down in columns.
--
-- # Ordering, which is the property that can corrupt rather than delay
--
-- Consumers FOLD these FACTs. tv-sync's transition() DROPS an ORDER_ROUTED for
-- an order its projection never admitted, so publishing ROUTED before ACCEPTED
-- does not merely arrive late — it leaves a projection permanently blind to a
-- live order. The relay therefore preserves order PER partition_key (order_id
-- for every OMS FACT), which is the granularity the bus keys on and the
-- granularity a consumer folds at. Nothing is promised across keys.
--
-- id is a BIGSERIAL and id order is the order the relay publishes in. A sequence
-- allocates at INSERT time, not COMMIT time, so in general id order and commit
-- order can disagree — transaction A takes id 5, B takes 6 and commits first,
-- and a relay reading between the two commits sees 6 alone. THAT CANNOT HAPPEN
-- FOR ONE partition_key HERE, and the reason is a constraint rather than a
-- coincidence: every enqueue rides a versioned write of the same aggregate.
-- Admission is INSERT ... ON CONFLICT DO NOTHING (one winner per order_id) and
-- every transition after it carries the CAS predicate on orders.version (#122),
-- so two transactions for one order cannot both commit — the loser is refused
-- and its outbox row rolls back with it. An enqueue that did NOT ride such a
-- write would break this silently, in the reordering direction. See the ordering
-- contract on outbox.Queue, which says the same thing where the code is.
--
-- The sequence is cluster-global rather than per tenant, so an id gap is weak
-- evidence of another tenant's write volume. A per-tenant counter would need its
-- own serialization point on the write path — the exact cost the sequence exists
-- to avoid — for an inference nobody can act on.
--
-- # Rows are kept, not deleted, once published
--
-- published_at is stamped in place. A DELETE would make "this FACT was announced
-- and here is when" unanswerable the moment it succeeded, and that question is
-- asked during exactly the incident this table exists for. Retention is an
-- operational decision with a real cost either way, and it is deliberately NOT
-- made here: nothing prunes this table yet, and a fund with a large order flow
-- will need a policy. Sizing it needs production volume, which does not exist.

-- # TWO TENANTS, AND THEY ARE NOT THE SAME TENANT
--
-- tenant_id is the STORAGE scope — whose rows these are, what RLS filters on. It
-- is the OMS's own serving tenant, taken from the connection exactly as
-- orders.tenant_id is, because the record has to live in the same isolation
-- domain as the order it announces.
--
-- envelope_tenant_id is what goes on the WIRE — the tenant the FACT is published
-- under, lifted off the inbound command's envelope.
--
-- ON THE DEPLOYMENT THAT EXISTS TODAY THESE DIFFER ON EVERY REAL ORDER, and
-- collapsing them would be a total outage rather than a subtle bug. The shipped
-- OMS runs OMS_TENANT="__system__" while the api-gateway stamps the CALLER's
-- tenant on the command; pkg/bus.RequireTenantScope documents that mismatch as
-- the accepted state of #223, inert until #97 gives each tenant its own compute.
-- So the order row is stored under __system__ while its ORDER_ACCEPTED FACT is
-- published under acme, and both are correct. Writing the envelope's tenant into
-- tenant_id would make the RLS WITH CHECK refuse the insert — every admission,
-- every order, on the deployment that exists now.
--
-- Do not "simplify" this to one column before #97 lands and OMS_TENANT is a real
-- tenant. The day they become equal is the day the merge is safe, and not before.
CREATE TABLE outbox (
    id                 BIGSERIAL   NOT NULL,
    tenant_id          TEXT        NOT NULL DEFAULT app_current_tenant(),  -- storage scope; RLS filters on this
    envelope_tenant_id TEXT        NOT NULL,   -- published tenant; see above, NOT the same thing
    partition_key      TEXT        NOT NULL,   -- order_id; the unit order is preserved within
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
    payload            BYTEA       NOT NULL,   -- marshaled domain message (exact decimals; see above)
    enqueued_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,            -- NULL ⇒ the estate has not heard this yet
    attempts           INTEGER     NOT NULL DEFAULT 0,
    last_error         TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- The relay's only read: unpublished records, by key, in id order. PARTIAL on
-- published_at IS NULL so the index holds the BACKLOG and not the history —
-- a published row leaves the index, so the drain stays the same cost on day 1000
-- as on day 1 no matter how many FACTs the fund has emitted.
CREATE INDEX outbox_pending_idx ON outbox (tenant_id, partition_key, id)
    WHERE published_at IS NULL;

-- Tenant isolation (MT-01d/e) on the STORAGE tenant, and here it is load-bearing
-- in a way it is not on the other tables: this one holds the FACTS THEMSELVES,
-- envelope and payload. A relay that could read another storage tenant's rows
-- would not merely read them — it would PUBLISH them onto the shared bus. That
-- is also why the relay runs in-process on the OMS's own tenant-scoped pool
-- rather than as one estate-wide binary: app_current_tenant() RAISES on an
-- unscoped session, the app role is NOSUPERUSER, and there is no cross-tenant
-- read to be had. See outbox.Relay for the full argument.
--
-- IT DOES NOT AND CANNOT CONSTRAIN envelope_tenant_id, and that is not an
-- oversight — see the two-tenant note above. Until #97, one __system__ OMS
-- legitimately holds records destined for many envelope tenants; an RLS
-- predicate over that column would refuse every one of them. The control that
-- keeps a caller from choosing an arbitrary envelope tenant is upstream, at the
-- gateway and at bus.RequireTenantScope, not here.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
