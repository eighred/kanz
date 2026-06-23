package compute_test

import (
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/compute"
)

// LATENCY-01d — the in-process regression guard for the LATENCY-01c compute
// optimizations, plus the p99-under-burst test. These run as ordinary `go test`
// cases (the kanz-ci `go test -race ./...` gate executes them every PR), and the
// allocation guards are deterministic — unlike a timing benchstat-vs-baseline
// gate, which is flaky on shared CI runners. The `testing.B` benchmarks they
// guard live in bench_test.go / decimal_bench_test.go.
//
// The 500ms p99 budget is the END-TO-END read SLO (ORCH-01f), guarded against a
// live stack by the LATENCY-01a k6 smoke. In-process compute is microseconds, so
// the budget here is deliberately generous — a catastrophic-regression catch (an
// accidental O(n²) or a lock-convoy), not the real SLO.

// TestPositions_WarmCacheIsZeroAllocs locks LATENCY-01c F1: once materialized,
// repeated Positions() within a query returns the memoized slice with no
// allocation. A regression (dropping the cache, re-sorting per call) would make
// this non-zero and balloon the measures query's allocation count.
func TestPositions_WarmCacheIsZeroAllocs(t *testing.T) {
	p := buildBenchPortfolio(1024)
	_ = p.Positions() // materialize the cache

	allocs := testing.AllocsPerRun(20, func() {
		positionSink = p.Positions()
	})
	if allocs != 0 {
		t.Errorf("warm Positions() allocated %.0f/op, want 0 — the sortedCache (F1) regressed", allocs)
	}
}

// TestComputeMeasures_AllocsConstantInN locks LATENCY-01c F2/F3: with a warm
// position cache the per-query allocation count is O(1) — independent of the
// position count — because the per-position Decimal fold accumulates without
// allocating (decAccum). If someone reintroduces a per-position allocation, the
// n=1024 count blows past the n=16 count and this fails.
func TestComputeMeasures_AllocsConstantInN(t *testing.T) {
	reg := compute.DefaultRegistry()

	measure := func(n int) float64 {
		p := buildBenchPortfolio(n)
		_ = compute.ComputeMeasures(p, reg, nil) // warm the cache
		return testing.AllocsPerRun(20, func() {
			measureSink = compute.ComputeMeasures(p, reg, nil)
		})
	}

	small := measure(16)
	large := measure(1024)

	// O(1): a 64× larger book must not cost materially more allocations. A small
	// slack absorbs map-iteration order noise; a per-position regression would be
	// hundreds of allocations apart.
	if large > small+4 {
		t.Errorf("ComputeMeasures allocs grew with n (%.0f at n=1024 vs %.0f at n=16) — per-position allocation regressed (F2/F3)", large, small)
	}
	// Absolute ceiling: the four DefaultRegistry measures each materialize a
	// couple of result Decimals; 32 is comfortable headroom over the ~8 observed.
	if large > 32 {
		t.Errorf("ComputeMeasures allocated %.0f/op at n=1024, want <= 32", large)
	}
}

// TestComputeMeasuresP99UnderBurst drives the measures query under a 10×
// CPU-oversubscribed burst and asserts the p99 stays within a generous
// in-process budget — the resilience check the task calls for (p99 held under
// 10× burst), catching a lock convoy or an accidental super-linear blowup.
func TestComputeMeasuresP99UnderBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("burst latency test skipped under -short")
	}
	const (
		// Generous vs the ~tens-of-ms observed: p99 under 10× oversubscription is
		// dominated by goroutine scheduling + GC jitter, not compute, so the
		// budget catches catastrophic regressions (O(n²), a lock convoy → seconds)
		// without flaking on a slow runner. Still well under the 500ms read SLO.
		budget    = 250 * time.Millisecond
		perWorker = 200
		burst     = 10 // 10× oversubscription
	)
	reg := compute.DefaultRegistry()
	base := buildBenchPortfolio(256)

	workers := burst * runtime.GOMAXPROCS(0)
	lat := make([]time.Duration, workers*perWorker)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				start := time.Now()
				// Clone per call mirrors the real per-query path (Snapshot →
				// Clone → compute). Concurrent Clone()s only read base's map.
				ms := compute.ComputeMeasures(base.Clone(), reg, nil)
				lat[w*perWorker+i] = time.Since(start)
				if ms == nil { // use the result, defeat elimination, no shared write (race-free)
					t.Error("ComputeMeasures returned nil")
				}
			}
		}(w)
	}
	wg.Wait()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[int(float64(len(lat))*0.99)]
	t.Logf("compute p99 under %d× burst (%d workers, %d samples) = %v", burst, workers, len(lat), p99)
	if p99 > budget {
		t.Errorf("compute p99 under %d× burst = %v, want <= %v", burst, p99, budget)
	}
}
