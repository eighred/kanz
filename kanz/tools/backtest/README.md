# backtest (LAKE-01c/e)

Strategy backtest harness. Drives a historical event stream — the EVT-20 replay
`Source` — through a deterministic `Strategy`, exposing to each decision only the
knowledge knowable at that event's `event_time` (the LAKE-01b point-in-time
materializer pinned to an `AsOf` horizon).

Because the strategy computes its features with the **same** code the live engine
uses (`compute` over the MODEL-01b bitemporal store) and sees exactly what it saw
live, a backtest reproduces the live decision stream **bit-for-bit** — the
EVT-21d determinism property.

## Shape

```go
type Strategy interface {
    OnEvent(ctx, Event, PointInTime) ([]Decision, error)
}

h := &backtest.Harness{Materializer: dataset.NewMaterializer(store, featureSource, cfg)}
res, err := h.Run(ctx, src, strategy) // src: replay.Reader or backtest.NewSliceSource(...)
```

The harness is offline and single-threaded: it `Collect`s the whole range into a
deterministic order (event_time, then partition, then offset) before processing,
so a run is reproducible regardless of Kafka's cross-partition arrival order. A
strategy must be deterministic — no wall-clock, no unseeded RNG.

## Tests (LAKE-01e)

`go test ./tools/backtest/...`:

- **determinism** — the same range run twice yields an identical decision slice.
- **deterministic ordering** — a shuffled source yields the same stream as an
  in-order one (Collect sorts by event_time).
- **backtest reproduces live** — a late restatement (learned after every event's
  event_time) is invisible to the point-in-time reads, so the backtest decisions
  equal the live decisions; the same correction becomes visible once its
  knowledge horizon passes, proving the bitemporal read is correct both ways.

## BTCUSDT paper experiment

From `kanz/`, evaluate an archived Binance Spot Testnet BTCUSDT five-minute
kline response for one completed UTC day.

```sh
go run -mod=readonly ./tools/backtest/cmd/testnet_paper \
  -day 2026-11-20 -input /path/to/btcusdt-5m.json
```

Use the 12-field rows returned by the public `GET /api/v3/klines` endpoint.
Preserve the original JSON and its request metadata. Request the 20 intervals
before midnight and all 288 intervals of the evaluation day in one file. The
command rejects missing, duplicate, unfinished, inactive or out-of-order bars
and inconsistent OHLC bounds. Kline rows omit the venue, symbol and historical
publication times. A contiguous response establishes the observed bar window.
Source identity, revision time and live feed uptime require separate evidence.
The report includes the SHA-256 of the exact input bytes.

The fixed diagnostic starts in cash with 1,000 simulated USDT. If the mean of
the last five closes exceeds the mean of the last twenty, hold BTC. Otherwise
hold cash. Only closes reach the replay strategy. Each signal can trade at the
next five-minute open, including the first open after warm-up. Buy as much BTC
as available cash can fund, rounded down to a simulated step of 0.00000001 BTC.
A sale closes the full position. Each simulated fill incurs 5 basis points
of adverse slippage from the open and a 10 basis point quote-currency fee on
the slipped notional. The assumptions include full fills, sufficient depth
and immediate availability of the prior close. Actual latency, spreads,
exchange lot filters, partial fills and account-specific fees are unmeasured.

The report retains the ordered fills, cash and BTC, and discloses fees and
slippage. Marked equity uses the last close. Net P&L and return additionally
deduct hypothetical exit slippage and fees on any remaining BTC. Maximum
drawdown measures marked equity at five-minute closes against its prior peak,
including initial cash. Intrabar excursions are outside that measure.
All calculation uses exact decimal or rational arithmetic before display
rounding. The experiment has no account connection, order submission, fitted
model or calibrated probability. A single day's simulated P&L is an observation
on this input and cost model.
