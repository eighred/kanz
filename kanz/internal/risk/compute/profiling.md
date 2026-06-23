# Compute hot-path profiling (LATENCY-01b)

Profile of the RISK-06/07 read-query compute paths — `ComputeExposure`,
`ComputeMeasures`, and the Decimal arithmetic + `Portfolio.Positions` substrate
they lean on. This documents *where the time and allocations go*; the
optimizations it motivates are **LATENCY-01c**, and the CI regression guard over
the benchmarks is **LATENCY-01d**.

## Method

Benchmarks live alongside the code: `bench_test.go` (black-box, exported paths)
and `decimal_bench_test.go` (white-box, the unexported `addDecimal`/`mulDecimal`/
`absDecimal`/`sumInBaseCurrency` helpers). Portfolio sizes 16 / 256 / 1024
positions span a small book to a large institutional portfolio.

```sh
go test -run '^$' -bench . -benchmem ./internal/risk/compute/
go test -run '^$' -bench 'BenchmarkComputeMeasures/n=1024' -benchmem \
    -memprofile mem.out -cpuprofile cpu.out ./internal/risk/compute/
go tool pprof -top -sample_index=alloc_space mem.out
go tool pprof -top cpu.out
```

Numbers below are from a 12th-gen i5-12500H (`go test -benchmem`); treat them as
*relative* — absolute ns/op vary by machine, the ratios + alloc counts are the
signal. They are the baseline LATENCY-01d benchstats against.

## Headline numbers

| Benchmark | ns/op | B/op | allocs/op |
|---|--:|--:|--:|
| `AddDecimal` (aligned / misaligned) | ~24 | 64 | 1 |
| `MulDecimal` / `AbsDecimal` | ~21 | 64 | 1 |
| `SumInBaseCurrency` n=1024 | 200,865 | 155,880 | 1,029 |
| `Positions` n=1024 | 177,136 | 90,280 | 4 |
| `ComputeExposure` n=1024 | 400,025 | 639,818 | 6,161 |
| `ComputeMeasures` n=1024 | 1,511,608 | 952,450 | 3,624 |

`ComputeMeasures` `pprof` at n=1024 (alloc_space / cpu):

```
alloc_space  74.9%  domain.(*Portfolio).Positions
             23.7%  compute.addDecimal               (≈98.7% of all allocations)
cpu          69.2%  domain.(*Portfolio).Positions    (sort.partition_func +
                    cmpbody + insertionSort + reflectlite.Swapper + memmove)
```

## Findings

### F1 — `Portfolio.Positions()` is recomputed and re-sorted per measure (dominant)

`Positions()` allocates a fresh `[]Position` and runs an `O(n log n)`
`sort.Slice` (a reflection-based closure swap — `reflectlite.Swapper` shows up
hot) on **every call**. A single `ComputeMeasures` over `DefaultRegistry` calls
it **~8×**: each of the four measures (Gross/Net/VaR99/Delta) calls both
`sumInBaseCurrency` and `sumUncertaintyInBaseCurrency`, and each of those calls
`p.Positions()`. `ComputeExposure` adds one more. That single fan-out is **75%
of all allocations and ~69% of CPU** in a measures query.

The sort is pure overhead for the aggregation: summing is order-independent, and
the only consumers that need a stable order — `ExposureSet`/`MeasureSet` — sort
on read anyway. We pay a reflection sort 8× to feed loops that don't care about
order.

→ **01c:** compute the position slice **once per query** and thread it through
(e.g. measures take a precomputed `[]Position`, or the engine snapshots
positions once and hands the same slice to `ComputeExposure` + every
`MeasureFunc`). Drop the in-`Positions` sort, or offer an unsorted
`positionsUnsorted()` for the compute path. Expected: ~8× → 1× on both the
slice-alloc and the sort, collapsing the bulk of F1.

### F2 — Decimal helpers heap-allocate one proto `Decimal` per operation

Every `addDecimal`/`mulDecimal`/`absDecimal` returns a freshly-allocated
`*commonpb.Decimal` (64 B — proto message overhead, not 16 B). The measure inner
loop sums with `sum = addDecimal(sum, amt)`, so an `n`-position sum is `O(n)`
heap allocations: `SumInBaseCurrency` n=1024 ≈ 1,029 allocs. This is the 24%
`addDecimal` slice of the profile.

→ **01c:** accumulate in a value-type `(coefficient int64, exponent int32)` (or
a small unexported `decAccum` struct) and materialize a single `*Decimal` at the
end of the loop — turning `O(n)` allocations into `O(1)`. Exponent alignment
stays identical (track the running min exponent). Keep the existing helpers for
one-shot call sites.

### F3 — `ComputeExposure` allocates ~6 objects per position

Per position it builds an instrument `Exposure` (`absMoney` → `Money` + `Decimal`),
then folds into a currency bucket via `addMoney` (another `Money` + `Decimal`).
At n=1024 that's 6,161 allocs / 640 KB. The currency-key map + `sort.Strings` is
*not* a hot spot (few distinct currencies); the per-position Money/Decimal churn
is.

→ **01c:** same value-accumulator idea for the per-currency buckets (sum into a
plain accumulator keyed by currency, materialize `Money` once per bucket), and
reuse the F1 shared position slice. Lower priority than F1/F2.

### F4 — minor

- `ComputeMeasures`'s `want` filter does a linear scan of the filter list per
  measure — negligible at realistic filter sizes; skip unless it shows up later.
- `addDecimal`'s aligned vs misaligned cost is the same (~24 ns) — `pow10` is not
  a hot spot, so leave the alignment logic alone.

## Perspective

At today's placeholder math, `ComputeMeasures` n=1024 is ~1.5 ms — well inside
the 500 ms read budget for a single query. The reasons to fix it anyway:

1. **GC pressure.** ~950 KB and ~3,600 allocs *per query* (and the recompute path
   runs this on every debounced state change, not just on reads) is allocation
   churn that drives GC and tail-latency under load — exactly what the LATENCY-01
   p99 budget guards.
2. **It scales with the book and the model.** MODEL-01's real VaR/factor measures
   do far more per position than the placeholders; the `Positions()` fan-out and
   per-step Decimal allocs compound as both `n` and the per-position work grow.

F1 then F2 are the high-leverage 01c targets (≈99% of the allocations between
them); F3 follows; F4 is noise.

## Results (LATENCY-01c)

Implemented F1–F3:

- **F1** — `domain.Portfolio` lazily memoizes the sorted `Positions()` slice,
  invalidated on every mutation. Within a query the ~8 calls now sort + allocate
  once, not eight times. Safe without a lock: every reader holds a per-query
  `Clone` (state.Store.Snapshot) owned by one goroutine.
- **F2/F3** — an allocation-free `decAccum` (running coefficient/exponent)
  replaces the per-position `addDecimal`/`addMoney` fold in `sumInBaseCurrency`
  and `ComputeExposure`'s currency buckets: `O(n)` Decimal allocations → `O(1)`.
  Results are byte-identical (the accumulator's zero value == the old
  `zeroDecimal()` seed), so no measure value changes.

The benchmarks now **clone per iteration** to mirror the real per-query path
(`Snapshot → Clone → compute`, cold cache), so the numbers below *include* the
snapshot clone — a truer figure than the 01b table (which reused one fixture and
omitted the clone). Allocation count is the cleanest cross-comparable metric.

| Benchmark | 01b ns/op | 01c ns/op | 01b allocs | 01c allocs |
|---|--:|--:|--:|--:|
| `SumInBaseCurrency` n=1024 (sum only) | 200,865 | 5,140 | 1,029 | 1 |
| `ComputeExposure` n=1024 | 400,025 | 345,481 | 6,161 | 2,071 |
| `ComputeMeasures` n=1024 | 1,511,608 | 301,015 | 3,624 | 19 |

`ComputeMeasures` n=1024: **3,624 → 19 allocs/op, ~5× faster** (and the 01b
baseline didn't even pay the clone the 01c run does). The measure arithmetic is
now effectively free; what remains per query is the **snapshot `Clone` (map copy)
plus the single position sort** — `ComputeMeasures` ≈ `ComputeExposure` ≈
`Positions(cold)` in bytes, i.e. the residual is the clone+sort, not compute.

**Remaining (not pursued — out of LATENCY-01c's measured scope):**

- The per-query `Clone` map-copy + the one `O(n log n)` sort now dominate. Both
  are state-layer, not compute; a single-writer/lock-free state redesign (the
  RISK-05 alternative) or a copy-on-write sorted position list would be the next
  lever, but that is a larger structural change to weigh on its own.
- `ComputeExposure`'s per-instrument `Money` (≈2,071 allocs at n=1024) is genuine
  output — one exposure object per instrument — not folding overhead.
- `sumUncertaintyInBaseCurrency`'s `[]*Decimal` slice + `PropagateSumIndependent`
  fold still allocate when uncertainty bands are populated (nil in these
  benches); the F1 cache already removed its dominant `Positions()` cost. Left
  single-sourced with the public propagators.
