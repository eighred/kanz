-- 0001: durable IBOR ledger (PARITY-02a).
--
-- The accounting book-of-record is event-sourced: an append-only journal of
-- bitemporal LedgerEntry rows is the source of truth, and the Book is their
-- fold. A periodic snapshot (PERS-01 stance) bounds replay to the journal tail.
-- Same journal ⇒ same book ⇒ same NAV, so the durable store changes nothing
-- about the fold (internal/ledger); it only makes the journal survive a restart.
--
-- # Exact decimals
--
-- Money and quantity are exact base-10 rationals (*big.Rat) — `double` is banned
-- for money/sizes/prices (KANZ_BRAIN, EVT-14). They are stored as their lossless
-- RatString ("num/den") in TEXT; the book is the only reader, the text is its
-- contract with itself (the PERS-01 opaque-bytes rationale, in human-readable
-- form). Corporate-action parameters and the snapshot maps are JSONB with the
-- same string-encoded rationals.
--
-- # Bitemporal read
--
-- Each entry has an effective time (when it economically happened) and a
-- knowledge time (when the book learned of it). ReplayAsOf is served by the
-- (portfolio, effective, knowledge, entry) index below, so a point-in-time read
-- is an indexed range scan, not a full-journal scan.

CREATE TABLE ledger_entries (
    tenant_id      TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    entry_id       TEXT        NOT NULL,
    portfolio_id   TEXT        NOT NULL,
    entry_type     INTEGER     NOT NULL DEFAULT 0,   -- ledger.EntryType
    instrument_id  TEXT        NOT NULL DEFAULT '',
    quantity       TEXT,                              -- *big.Rat RatString, null for a pure cash leg
    price          TEXT,                              -- *big.Rat RatString
    cash           TEXT,                              -- *big.Rat RatString, null for no cash effect
    cash_currency  TEXT        NOT NULL DEFAULT '',
    action         JSONB,                             -- corporate-action params, null otherwise
    effective_time TIMESTAMPTZ NOT NULL,
    knowledge_time TIMESTAMPTZ NOT NULL,
    source_ref     TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, entry_id)                 -- idempotent append on entry_id
);

-- The bitemporal fold order and the ReplayAsOf range scan: entries for a
-- portfolio, ordered (and bounded) by (effective, knowledge, entry_id).
CREATE INDEX ledger_entries_bitemporal_idx
    ON ledger_entries (tenant_id, portfolio_id, effective_time, knowledge_time, entry_id);

CREATE TABLE ledger_snapshots (
    tenant_id    TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    portfolio_id TEXT        NOT NULL,
    positions    JSONB       NOT NULL DEFAULT '{}',  -- instrument -> {qty,avg,realized} (RatStrings)
    cash         JSONB       NOT NULL DEFAULT '{}',  -- currency -> RatString
    accrued      JSONB       NOT NULL DEFAULT '{}',  -- currency -> RatString
    through_time TIMESTAMPTZ NOT NULL,               -- knowledge watermark folded through
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, portfolio_id)
);

-- Tenant isolation via row-level security (MT-01d): deny-by-default, the
-- session GUC app.tenant_id scopes every read/write. A connection with the GUC
-- unset sees no rows (current_setting(...,true) is NULL ⇒ predicate NULL ⇒
-- fail-closed). The engine must connect as a non-superuser role.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['ledger_entries', 'ledger_snapshots'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE  ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = current_setting('app.tenant_id', true))
        WITH CHECK (tenant_id = current_setting('app.tenant_id', true))
    $f$, t);
  END LOOP;
END $$;
