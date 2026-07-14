//go:build perf

// This file is BEHIND A BUILD TAG, and that is the whole point of it.
//
// It holds a WALL-CLOCK LATENCY BUDGET, which is not a correctness assertion and does
// not belong in the correctness suite. It lived in regression_test.go and ran on every
// `go test ./...`, where it measured the machine rather than the code: under the race
// detector (5-20x overhead, and CI runs `go test -race ./...`) and under ordinary
// full-suite CPU contention it blew its 250ms budget routinely — 482ms and 852ms on an
// idle developer box simply because other packages were compiling alongside it. A guard
// that fails for reasons unrelated to the thing it guards gets ignored, then deleted,
// and then it guards nothing.
//
// .github/workflows/latency.yml already said this is how it should be: "the
// deterministic allocation-regression GUARD is the testing.AllocsPerRun cases in
// compute/regression_test.go ... (no flaky timing baseline)". This WAS the flaky timing
// baseline that comment disclaims. It now runs where a timing number means something —
// alone, unraced, in the latency workflow:
//
//	go test -tags perf -run TestComputeMeasuresP99UnderBurst ./internal/risk/compute/...
//
// The hot path keeps its CI coverage without it: the AllocsPerRun guards (deterministic,
// in the -race gate) and the testing.B benchmarks the latency workflow runs per PR.
package compute_test

import (
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/compute"
)

// TestComputeMeasuresP99UnderBurst drives the measures query under a 10×
// CPU-oversubscribed burst and asserts the p99 stays within a generous in-process
// budget — catching a lock convoy or an accidental super-linear blowup.
func TestComputeMeasuresP99UnderBurst(t *testing.T) {
	const (
		// Generous vs the ~tens-of-ms observed: p99 under 10× oversubscription is
		// dominated by goroutine scheduling + GC jitter, not compute, so the budget
		// catches catastrophic regressions (O(n²), a lock convoy → seconds) without
		// flaking. Still well under the 500ms read SLO.
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
				// Clone per call mirrors the real per-query path (Snapshot → Clone →
				// compute). Concurrent Clone()s only read base's map.
				ms := compute.ComputeMeasures(base.Clone(), reg, nil)
				lat[w*perWorker+i] = time.Since(start)
				if ms == nil { // use the result, defeat elimination, no shared write
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
