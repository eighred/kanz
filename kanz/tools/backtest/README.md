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
