-- MODEL-01b: market-data time-series store — historical price observations.
--
-- Bitemporal by design (MODEL-01i point-in-time correctness): observation_time
-- is the time a price applies to, knowledge_time is when Kanz learned it. The
-- primary key is the full bitemporal identity, so a restatement (same
-- observation_time, newer knowledge_time) coexists with the original rather
-- than overwriting it — a backtest as of T reads only knowledge_time <= T and
-- never sees a later correction. Prices are stored as marshaled
-- common.v1.Decimal bytes (exact base-10; double is banned for prices).
--
-- NOT tenant-scoped: price history is universal market fact, not tenant-owned
-- state, so no tenant_id / RLS (same call the schema registry made). Tenant
-- isolation lives on the portfolio state that consumes these prices.

CREATE TABLE IF NOT EXISTS price_observations (
    instrument_id    TEXT        NOT NULL,
    observation_time TIMESTAMPTZ NOT NULL,
    kind             INTEGER     NOT NULL,           -- reference.v1.PriceKind
    price            BYTEA       NOT NULL,           -- marshaled common.v1.Decimal
    currency_code    TEXT,                           -- ISO 4217; NULL until reference-data join
    knowledge_time   TIMESTAMPTZ NOT NULL,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, observation_time, kind, knowledge_time)
);

-- The hot read is History(instrument, kind, observation_time window, knowledge
-- horizon) → DISTINCT ON (observation_time, kind) ORDER BY knowledge_time DESC.
-- This index serves both the window scan and the per-date latest-known collapse.
CREATE INDEX IF NOT EXISTS price_observations_series_idx
    ON price_observations (instrument_id, kind, observation_time, knowledge_time DESC);

-- TimescaleDB optimization: make it a hypertable partitioned on observation_time
-- when the extension is installed. Plain PostgreSQL skips this and keeps the
-- regular indexed table — the store code is identical on either.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable('price_observations', 'observation_time',
                                  if_not_exists => TRUE, migrate_data => TRUE);
    END IF;
END
$$;
