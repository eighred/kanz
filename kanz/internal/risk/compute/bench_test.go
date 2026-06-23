package compute_test

import (
	"fmt"
	"testing"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// LATENCY-01b — profiling the RISK-06/07 compute hot paths.
//
// These benchmarks measure the read-query compute cost (ComputeExposure /
// ComputeMeasures) and the substrate they lean on (Portfolio.Positions). Run:
//
//	go test -run '^$' -bench . -benchmem ./internal/risk/compute/
//	go test -run '^$' -bench BenchmarkComputeMeasures -benchmem \
//	    -cpuprofile cpu.out -memprofile mem.out ./internal/risk/compute/
//	go tool pprof -top mem.out
//
// The findings + the optimizations they motivate (LATENCY-01c) are written up
// in profiling.md alongside. The CI regression guard over these benchmarks is
// LATENCY-01d (`testing.B` + benchstat).

// benchSizes are representative portfolio cardinalities: a small book, a
// mid-size desk, and a large institutional portfolio.
var benchSizes = []int{16, 256, 1024}

// Package sinks defeat dead-code elimination of the benchmarked results.
var (
	exposureSink *domain.ExposureSet
	measureSink  *domain.MeasureSet
	positionSink []domain.Position
)

// buildBenchPortfolio constructs an n-position portfolio in the base currency
// (USD), with deterministic instrument IDs + marks. ~1/8 of positions are in a
// non-base currency (EUR) to exercise the same-currency skip path the measures
// take, and exponents/signs vary so the addDecimal alignment + abs paths are hit.
func buildBenchPortfolio(n int) *domain.Portfolio {
	p := domain.NewPortfolio("BENCH", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: baseTime, BaseCurrency: "USD"})
	for i := 0; i < n; i++ {
		ccy := "USD"
		if i%8 == 0 {
			ccy = "EUR"
		}
		coef := int64((i + 1) * 1000)
		if i%3 == 0 {
			coef = -coef
		}
		p.SetPosition(domain.Position{
			InstrumentID: v1.InstrumentID(fmt.Sprintf("INS%06d", i)),
			MarketValue:  mkMoney(coef, int32(-(i % 4)), ccy),
			AsOf:         baseTime,
		})
	}
	return p
}

// ComputeExposure / ComputeMeasures clone the portfolio per iteration to mirror
// the engine's real per-query path (state.Store.Snapshot → Clone → compute): a
// fresh clone starts with a cold position cache, so each iteration pays the
// single Positions() materialization a real query does — not the warm-cache
// residual a reused portfolio would show. The within-query win (Positions
// computed once and shared across the ~8 internal calls, LATENCY-01c F1) is
// captured; the cross-query amortization a reused fixture would falsely add is not.

func BenchmarkComputeExposure(b *testing.B) {
	for _, n := range benchSizes {
		base := buildBenchPortfolio(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				exposureSink = compute.ComputeExposure(base.Clone())
			}
		})
	}
}

func BenchmarkComputeMeasures(b *testing.B) {
	reg := compute.DefaultRegistry()
	for _, n := range benchSizes {
		base := buildBenchPortfolio(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				measureSink = compute.ComputeMeasures(base.Clone(), reg, nil)
			}
		})
	}
}

// BenchmarkPositions isolates the cold-cache Portfolio.Positions cost — the
// slice-alloc + sort a query pays once (then reuses, post-LATENCY-01c). Clones
// per iteration so every call sees a cold cache.
func BenchmarkPositions(b *testing.B) {
	for _, n := range benchSizes {
		base := buildBenchPortfolio(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				positionSink = base.Clone().Positions()
			}
		})
	}
}
