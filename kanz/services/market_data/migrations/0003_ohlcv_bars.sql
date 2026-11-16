-- OHLCV bar series (#425) — the data foundation the alpha direction waits on.
--
-- market.v1.Bar has carried open, high, low, close, volume and trade_count all
-- along, and ingest kept only the close (internal/marketdata/ingest.go). So the
-- platform could answer "what did BTC-USDT last trade at" and not "what did it
-- do yesterday" — which is the question every indicator, model feature and
-- backtest is made of.
--
-- BITEMPORAL, exactly as price_observations is, and for the same reason:
-- bucket_start is the interval the candle applies to, knowledge_time is when
-- Kanz learned it. A venue restating a candle is a NEW knowledge_time coexisting
-- with the original, never an overwrite — so a backtest as of T reads only
-- knowledge_time <= T and a correction that arrived afterwards cannot leak
-- backwards. A store that overwrote would make every historical evaluation
-- quietly optimistic, in the direction nobody checks.
--
-- NOT tenant-scoped, the same call price_observations made: a candle is
-- universal market fact, not tenant-owned state. Tenant isolation lives on the
-- portfolio state that consumes these prices, not on the prices.
--
-- VENUE IS IN THE PRIMARY KEY, and that is the one thing this table does that
-- price_observations does not.
--
-- A bar from one exchange is not a bar from another: different books, different
-- last trades, a different close for the same minute. A model trained on a
-- composite candle and then executed against one venue's book shows a
-- backtest/live divergence that reads as "the strategy stopped working" rather
-- than as a data defect. #407 established the same rule one field over — this
-- platform does not smooth over which venue a price came from.
--
-- RESOLUTION IS IN THE KEY TOO. A 1-minute candle and the 1-hour candle covering
-- it are different observations of the same market at the same instant; one
-- series holding both answers "what happened at 12:00" with two rows and no way
-- to say which was meant. 1m is the base and the coarser ones are DERIVED from
-- it — never ingested independently, because two sources for one candle is two
-- answers to one question.

CREATE TABLE IF NOT EXISTS ohlcv_bars (
    instrument_id  TEXT        NOT NULL,
    venue          TEXT        NOT NULL,           -- ISO 10383 MIC
    resolution     TEXT        NOT NULL,           -- '1m' | '1h' | '1d'
    bucket_start   TIMESTAMPTZ NOT NULL,           -- inclusive start; the observation time

    -- Prices and volume are marshaled common.v1.Decimal bytes — exact base-10,
    -- as everywhere else on this platform. A candle's close becomes a mark, a
    -- mark values an order, and a float rounding error there is a wrong number
    -- that looks right.
    open           BYTEA       NOT NULL,
    high           BYTEA       NOT NULL,
    low            BYTEA       NOT NULL,
    close          BYTEA       NOT NULL,
    volume         BYTEA       NOT NULL,

    -- ZERO IS MEANINGFUL: an interval in which nothing traded is a real
    -- observation, not a gap. An indicator that cannot tell the two apart
    -- invents movement where the market was simply quiet.
    trade_count    BIGINT      NOT NULL DEFAULT 0,

    knowledge_time TIMESTAMPTZ NOT NULL,
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (instrument_id, venue, resolution, bucket_start, knowledge_time),
    CONSTRAINT ohlcv_bars_trade_count_nonneg CHECK (trade_count >= 0)
);

-- The hot read is Bars(instrument, venue, resolution, window, knowledge horizon)
-- → DISTINCT ON (bucket_start) ORDER BY knowledge_time DESC. This serves both
-- the window scan and the per-bucket latest-known collapse, in that order.
CREATE INDEX IF NOT EXISTS ohlcv_bars_series_idx
    ON ohlcv_bars (instrument_id, venue, resolution, bucket_start, knowledge_time DESC);

-- TimescaleDB when the extension is present, plain indexed table otherwise —
-- the store code is identical on either, exactly as 0001 does for prices.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable('ohlcv_bars', 'bucket_start',
                                  if_not_exists => TRUE, migrate_data => TRUE);
    END IF;
END $$;
