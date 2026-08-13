-- "THE MARKET WAS QUIET" AND "THE VENUE DID NOT TELL US" MUST NOT BE THE SAME ROW
-- (#432).
--
-- 0003 created trade_count as NOT NULL DEFAULT 0, and stated in a comment beside
-- it exactly why that mattered:
--
--     ZERO IS MEANINGFUL: an interval in which nothing traded is a real
--     observation, not a gap. An indicator that cannot tell the two apart
--     invents movement where the market was simply quiet.
--
-- The comment was right and the column could not honour it. OKX candles carry no
-- trade count at all — the row is [ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm],
-- there is no field to map — so the backfill wrote 0 for every OKX bar, and a
-- minute in which OKX saw thousands of trades was stored as a minute in which
-- nothing traded. The two states the comment insists must stay distinguishable
-- were one value.
--
-- WHY THIS IS WORTH ALTERING A SHIPPED COLUMN. Nothing reads trade_count today,
-- so nothing is producing wrong numbers right now — the damage is RETROACTIVE.
-- The bars being backfilled now are the history a model trains on later, and by
-- the time something does read this the OKX portion would be years deep of
-- permanently dead market. Re-fetching backfilled history is expensive where it
-- is possible at all, and some venues expire it. The cheap moment to fix this is
-- while the series is young.
--
-- WHY NULL AND NOT A SENTINEL. -1 would put "unknown" inside the value domain,
-- where every future reader has to remember to exclude it — and the reader that
-- forgets gets a plausible-looking negative in an average rather than an error.
-- NULL cannot be averaged by accident; SQL's aggregates skip it, which is the
-- behaviour every consumer wants and none of them has to remember to ask for.
--
-- SAFE AND BACKWARD-COMPATIBLE. Dropping NOT NULL widens what the column
-- accepts; every existing row keeps the value it has. This deliberately does NOT
-- rewrite historical OKX zeros to NULL: this migration cannot tell an OKX bar
-- written before the fix from a genuinely quiet Binance minute without encoding
-- source-specific knowledge into a schema change, and guessing wrong in either
-- direction is worse than leaving the pre-fix rows as the known-imperfect
-- history they are. New writes are correct from here.

ALTER TABLE ohlcv_bars
    ALTER COLUMN trade_count DROP NOT NULL,
    ALTER COLUMN trade_count DROP DEFAULT;

-- The CHECK from 0003 already tolerates NULL — in SQL a CHECK passes when its
-- expression is UNKNOWN — so `trade_count >= 0` keeps refusing a negative while
-- admitting "not reported". Restated here because relying on that silently is
-- how the next person removes it as dead.
COMMENT ON COLUMN ohlcv_bars.trade_count IS
    'Trades aggregated in the interval. NULL = the venue does not report one (OKX). '
    '0 = the venue reported that nothing traded. These are different observations '
    'and #432 exists because they used to be the same row.';
