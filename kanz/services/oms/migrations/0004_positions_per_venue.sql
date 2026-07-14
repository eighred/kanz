-- 0004: a holding sits AT A VENUE (EXEC-M19a).
--
-- THE PLATFORM COULD NOT ANSWER "HOW MUCH BTC DO WE HOLD AT OKX".
--
-- 0003 keyed the book on (tenant, portfolio, instrument). But a fund that holds 1 BTC at
-- Binance and 2 BTC at OKX holds TWO DIFFERENT THINGS: you cannot sell the OKX BTC on
-- Binance. With no venue in the key, the second venue's fill OVERWROTE the first — the
-- book said 2 BTC and the fund had 3, and nothing anywhere could say where they were.
--
-- What it cost: webhook-ingest's CLOSE path sizes each venue leg from the position AT THAT
-- VENUE (translate.legSizeAndSide), and there was no such number to give it — so it was
-- wired to an EMPTY map, a `close` alert produced ZERO orders, and the webhook answered
-- 202 Accepted. The trading loop could OPEN a position and could not CLOSE one.
--
-- The fill has carried the venue all along (order.v1.Fill.venue). The projector threw it
-- away.
--
-- # The fund-level view is not lost — it is DERIVED
--
-- Risk and compliance want ONE number per instrument: a fund's exposure does not care which
-- exchange holds it. They keep getting exactly that, because the aggregate is now SUMMED
-- from this table rather than stored as a second copy of it. One book, two projections;
-- two tables would be two truths, and the day they disagreed nobody would know which one
-- the fund actually held.
--
-- This is the same axis EXEC-M16 established for collateral: an exchange margins and
-- LIQUIDATES per account, and an account belongs to a venue.

-- An existing row has no venue to attribute it to. DEFAULT '' lets the column land, and the
-- default is dropped immediately after: from here on, a position with no venue is a fill
-- whose venue nobody recorded, and it must not be silently acceptable.
ALTER TABLE positions ADD COLUMN venue TEXT NOT NULL DEFAULT '';
ALTER TABLE positions ALTER COLUMN venue DROP DEFAULT;

-- The venue joins the identity of the holding.
ALTER TABLE positions DROP CONSTRAINT positions_pkey;
ALTER TABLE positions ADD PRIMARY KEY (tenant_id, portfolio_id, venue, instrument_id);

-- The aggregate read (the pre-trade gate's Snapshot, and the fund-level FACT) sums every
-- venue of one instrument, so it scans by (portfolio, instrument).
CREATE INDEX positions_instrument_idx ON positions (tenant_id, portfolio_id, instrument_id);
