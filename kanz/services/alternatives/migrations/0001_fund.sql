-- 0001: durable alternatives commitment journal (PARITY-02b).
--
-- The fund position is event-sourced (ALT-01b): an append-only journal of
-- commitment lifecycle events (Commit/Call/Distribution/NAVMark) that
-- internal/alternatives.Replay folds into a point-in-time Position. The durable
-- store changes nothing about the fold — same journal ⇒ same position, IRR,
-- TVPI, J-curve. It is the same shape as the accounting (IBOR) ledger store.
--
-- Amounts are exact base-10 rationals (*big.Rat) — `double` is banned for money
-- (EVT-14) — stored as their lossless RatString in TEXT; the fold is the only
-- reader. Append is idempotent on event_id (PK).

CREATE TABLE fund_events (
    tenant_id     TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    event_id      TEXT        NOT NULL,
    commitment_id TEXT        NOT NULL,
    event_type    INTEGER     NOT NULL DEFAULT 0,   -- alternatives.EventType
    amount        TEXT,                              -- *big.Rat RatString (non-negative)
    event_date    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, event_id)                -- idempotent append on event_id
);

-- The per-commitment fold order (date, event_id), and the bootstrap scan over
-- distinct commitments.
CREATE INDEX fund_events_commitment_idx
    ON fund_events (tenant_id, commitment_id, event_date, event_id);

-- Tenant isolation via row-level security (MT-01d): deny-by-default, scoped to
-- the session GUC app.tenant_id; a connection with it unset sees no rows.
ALTER TABLE fund_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE fund_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fund_events
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
