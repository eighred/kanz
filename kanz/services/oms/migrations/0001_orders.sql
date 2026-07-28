-- 0001: durable order state (EXEC-M7c).
--
-- This table IS the atomic admission gate. Before it, order.Store was an
-- in-process map: Create's exactly-once guarantee was a sync.Mutex, so its
-- safety boundary ended at the process. Two OMS pods each admitted the same
-- order_id and each routed it to the venue — the state converged (Save is an
-- upsert, so it LOOKED idempotent) while the fund traded twice. The PRIMARY KEY
-- below moves that guarantee from a mutex to the storage engine, where it holds
-- across every replica, and lifts the single-replica pin on the deployment.
--
-- The admission contract (see internal/order/store.go):
--
--   * Create → INSERT ... ON CONFLICT (tenant_id, order_id) DO NOTHING, and the
--     writer reads RowsAffected: 1 ⇒ this delivery owns the order and may route
--     it; 0 ⇒ it lost the race and must not touch the venue. It is ONE statement.
--     A SELECT-then-INSERT would reintroduce the check-then-act window this
--     migration exists to close.
--   * Save → the same INSERT with DO UPDATE, the post-admission upsert. It does
--     NOT arbitrate between concurrent writers. This used to claim safety came
--     from "the bus partition_key serializes transitions per order"; that was
--     never true — nothing in the consumer reads partition_key for ordering, and
--     submit, amend and cancel arrive on three separate durables. Concurrent
--     writers are excluded by the service's per-order lock, which stops at the
--     process boundary; across replicas the last writer silently wins. Closing
--     that needs a version column and a CAS predicate on the UPDATE.
--
-- # Exact decimals
--
-- OrderState carries Money/Decimal (price, quantity, filled quantity), which are
-- exact base-10 rationals — `double` is banned on any path moving capital
-- (KANZ_BRAIN, EVT-14). There is no lossless native SQL column for them, so the
-- state is stored as marshaled order.v1.OrderState proto bytes (BYTEA): the same
-- opaque-descriptor pattern as risk-engine PERS-01 and the schema registry
-- (EVT-16a). The OMS is the only reader; the bytes are its contract with itself.
-- order_id and status are denormalized out of the blob for indexing and
-- operator inspection — the blob stays authoritative.

CREATE TABLE orders (
    tenant_id  TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    order_id   TEXT        NOT NULL,
    status     INTEGER     NOT NULL DEFAULT 0,  -- order.v1.OrderStatus, denormalized
    state      BYTEA       NOT NULL,            -- marshaled order.v1.OrderState (authoritative)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, order_id)           -- THE admission gate: one winner per order_id
);

-- Operator/bootstrap reads ("what is still working?") scan by status, not by id.
CREATE INDEX orders_status_idx ON orders (tenant_id, status);

-- Tenant isolation via row-level security (MT-01d): deny-by-default, the session
-- GUC app.tenant_id scopes every read/write. A connection with the GUC unset
-- sees no rows (current_setting(...,true) is NULL ⇒ predicate NULL ⇒ fail-closed).
-- The engine must connect as a non-superuser role.
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON orders
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
