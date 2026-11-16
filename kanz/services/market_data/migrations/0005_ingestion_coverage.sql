-- INGESTION COVERAGE (#591, #416) — which intervals this platform was actually
-- OBSERVING.
--
-- ohlcv_bars says what traded. It cannot say whether anyone was looking:
-- internal/marketedge/bars/fold.go emits NO BAR for a minute in which nothing
-- traded, because that process cannot tell "nothing traded" from "the feed was
-- down" or "we were rolling pods". internal/marketdata/rollup/driver.go ruled on
-- what that costs and named the missing piece:
--
--   "The fix does not belong here: it belongs to an ingestion-coverage record
--    that states which intervals were actually OBSERVED, which this platform
--    does not yet have."
--
-- This table is that record.
--
-- EVERY ROW IS FIRST-HAND. It is written by the process that HELD the venue
-- subscription, about that subscription, published as a market.v1 FACT
-- (internal/marketedge/coverage). Nothing derives a row here from ohlcv_bars and
-- nothing ever may: a record computed from the series it exists to vouch for
-- vouches for itself, and would turn a silent outage into a confident "the
-- market was quiet".
--
-- THERE IS NO BACKFILL FOR THIS TABLE, AND THERE MUST NEVER BE ONE. Nothing can
-- reconstruct whether a feed was live last July. Intervals older than the first
-- row here are UNKNOWN and must stay that way — a fabricated attestation is
-- worse than none, because none is honestly unknown while a fabrication is a lie
-- a calibration report prices in. This is the same ruling #345 makes about
-- invented quotes.
--
-- NOT tenant-scoped, the same call price_observations and ohlcv_bars made: which
-- feeds this platform was holding is universal market-data operations fact, not
-- tenant-owned state.
--
-- NOT BITEMPORAL, unlike ohlcv_bars, and that difference is deliberate. A venue
-- RESTATES a candle, so a bar needs every version to coexist under its own
-- knowledge_time. An attestation is never revised — it is only ever re-proven,
-- and two claims about one bucket are two subscriptions each speaking for
-- itself. The merge is max(observed_ns): a sound lower bound on what the
-- platform actually observed, where last-write-wins would silently retract
-- coverage that was genuinely proven.

CREATE TABLE IF NOT EXISTS ingestion_coverage (
    instrument_id  TEXT        NOT NULL,
    venue          TEXT        NOT NULL,           -- ISO 10383 MIC. Coverage is PER VENUE:
                                                   -- a live Binance subscription says nothing
                                                   -- about what OKX was doing.
    resolution     TEXT        NOT NULL,           -- '1m' — matches the bar series it explains
    bucket_start   TIMESTAMPTZ NOT NULL,           -- inclusive start of the attested interval

    -- How much of the interval the subscription was PROVEN live for, in
    -- nanoseconds. A LOWER BOUND, never an estimate: time the attestor could not
    -- prove is simply not credited, so a short value means "cannot vouch for the
    -- difference", never "the feed was down for it". Equal to the interval is the
    -- only value that explains a missing bar as a QUIET market.
    --
    -- The CHECK is the over-claim guard. An attestor cannot have proven more time
    -- than the bucket holds, and a row that said so would be the strongest
    -- possible claim about a window produced by a bug.
    observed_ns    BIGINT      NOT NULL,

    -- How many times the subscription was observed to FAIL inside the interval.
    -- Zero with a short observed_ns means the attestor simply stopped hearing
    -- from the socket; non-zero names a fault that was actually seen.
    breaks         INTEGER     NOT NULL DEFAULT 0,

    -- WHICH SUBSCRIPTION IS SPEAKING, e.g. 'binance:trades:BTCUSDT'. It is in the
    -- PRIMARY KEY because two subscriptions may cover one series and each can
    -- only speak for itself; collapsing them would make the platform's claim
    -- whichever pod published second. A claim nothing can be traced to cannot be
    -- audited, so it is NOT NULL and the store refuses an empty one.
    attestor       TEXT        NOT NULL,

    recorded_at    TIMESTAMPTZ NOT NULL,           -- when Kanz learned this attestation
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (instrument_id, venue, resolution, bucket_start, attestor),
    CONSTRAINT ingestion_coverage_observed_nonneg CHECK (observed_ns >= 0),
    CONSTRAINT ingestion_coverage_breaks_nonneg   CHECK (breaks >= 0)
);

-- The hot read is Coverage(instrument, venue, resolution, window) → every
-- attestor's row over a bucket range, joined against a bar window of the same
-- shape. It mirrors ohlcv_bars_series_idx so the two scans cost the same.
CREATE INDEX IF NOT EXISTS ingestion_coverage_series_idx
    ON ingestion_coverage (instrument_id, venue, resolution, bucket_start);

-- TimescaleDB when the extension is present, plain indexed table otherwise —
-- identical to 0003, and the store code is the same on either.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable('ingestion_coverage', 'bucket_start',
                                  if_not_exists => TRUE, migrate_data => TRUE);
    END IF;
END $$;
