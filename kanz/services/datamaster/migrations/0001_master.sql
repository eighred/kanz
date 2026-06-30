-- 0001: durable golden records + price-exception queue (PARITY-02b).
--
-- The data master resolves a golden SecurityMaster across vendor feeds and
-- detects price-quality breaks during arbitration (MASTER-01). Today both live
-- in memory; this makes them survive a restart with the SAME contract: the
-- golden record is replace-on-write (a resolution is the whole truth for an
-- instrument), and the exception queue is idempotent-add on a deterministic id
-- with an append-only human-override audit trail.
--
-- The golden record is a resolved analytics shape (no money type — prices live
-- on market.v1 elsewhere), stored as a JSONB blob keyed by canonical instrument
-- id. Exceptions are relational so the open-queue read and the override trail
-- are first-class.

CREATE TABLE golden_records (
    tenant_id     TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    instrument_id TEXT        NOT NULL,
    record        JSONB       NOT NULL,            -- the master.SecurityMaster snapshot
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, instrument_id)
);

CREATE TABLE exceptions (
    tenant_id     TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    exception_id  TEXT        NOT NULL,            -- deterministic id (idempotent re-detect)
    kind          TEXT        NOT NULL,            -- pricing.ExceptionKind
    instrument_id TEXT        NOT NULL,
    detail        TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL,            -- OPEN / OVERRIDDEN / RESOLVED
    detected_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, exception_id)          -- Add keeps the existing entry (idempotent)
);

CREATE INDEX exceptions_open_idx ON exceptions (tenant_id, status, exception_id);

-- Append-only override audit trail: who chose what and why, never mutated.
CREATE TABLE exception_overrides (
    tenant_id    TEXT        NOT NULL DEFAULT current_setting('app.tenant_id', true),
    exception_id TEXT        NOT NULL,
    seq          BIGINT      GENERATED ALWAYS AS IDENTITY,
    actor        TEXT        NOT NULL,
    reason       TEXT        NOT NULL,
    chosen_price DOUBLE PRECISION NOT NULL,        -- an oversight statistic, not a stored price (EVT-14)
    overridden_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, exception_id, seq),
    FOREIGN KEY (tenant_id, exception_id) REFERENCES exceptions (tenant_id, exception_id) ON DELETE CASCADE
);

-- Tenant isolation via row-level security (MT-01d): deny-by-default.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['golden_records', 'exceptions', 'exception_overrides'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE  ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY tenant_isolation ON %I
        USING (tenant_id = current_setting('app.tenant_id', true))
        WITH CHECK (tenant_id = current_setting('app.tenant_id', true))
    $f$, t);
  END LOOP;
END $$;
