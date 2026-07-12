-- 0001: the venue adapter's own order view (INFRA-M7a-2).
--
-- This is NOT a second copy of the OMS's order book, and it is not an admission
-- gate. The OMS admits an order (exactly once, at ITS primary key) and only then
-- calls this adapter. By the time a row lands here, the decision to trade has
-- already been made and is owned elsewhere.
--
-- What this table is: the adapter's record of what it was asked to work, so its
-- background workers can do their jobs after the process boundary cut them off
-- from the OMS store.
--
--   * The user-data websocket delivers an execution report carrying only a
--     clOrdId and a symbol. To publish a Kanz fill FACT, the adapter must enrich
--     it with the order's instrument, side, and terms. It reads them here.
--   * The reconciler must know which orders it BELIEVES are open at the venue, to
--     query each on the exchange and heal the drift. It reads them here.
--
-- Why durable and not a map: this is precisely the state a restart must not lose.
-- An adapter that reboots with an empty view cannot enrich a fill that arrives a
-- second later for an order it worked a second ago, and cannot tell its
-- reconciler what should be open. It would fall silent exactly when the healing
-- seam is most needed.
--
-- state is marshaled order.v1.OrderState (BYTEA): it carries exact base-10
-- Money/Decimal, `double` is banned on any path moving capital, and there is no
-- lossless native SQL column for it (the PERS-01 / EVT-16a opaque-bytes stance).

CREATE TABLE venue_orders (
    tenant_id  TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    order_id   TEXT        NOT NULL,
    status     INTEGER     NOT NULL DEFAULT 0,  -- order.v1.OrderStatus, denormalized
    state      BYTEA       NOT NULL,            -- marshaled order.v1.OrderState (authoritative)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, order_id)
);

-- The reconciler's read is "everything not terminal", so it scans by status.
CREATE INDEX venue_orders_status_idx ON venue_orders (tenant_id, status);

-- Tenant isolation via row-level security (MT-01d): deny-by-default, the session
-- GUC app.tenant_id scopes every read/write. A connection with the GUC unset sees
-- no rows (current_setting(...,true) is NULL ⇒ predicate NULL ⇒ fail-closed). The
-- adapter must connect as a non-superuser role.
ALTER TABLE venue_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE venue_orders FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON venue_orders
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
