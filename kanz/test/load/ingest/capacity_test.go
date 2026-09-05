package main

import (
	"bufio"
	"math"
	"strings"
	"testing"

	"github.com/eighred/kanz/test/load/internal/promscrape"
)

// A representative risk-engine scrape: the two #1050 gauges, the runtime series
// and the recompute histogram, in the shapes client_golang actually exports.
const engineScrapeText = `# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 87
go_memstats_sys_bytes 4.194304e+07
process_resident_memory_bytes 2.68435456e+08
kanz_risk_recompute_inflight 4
kanz_risk_recompute_queue_depth 132
kanz_risk_recompute_duration_seconds_bucket{le="0.001"} 10
kanz_risk_recompute_duration_seconds_bucket{le="0.005"} 50
kanz_risk_recompute_duration_seconds_bucket{le="0.01"} 90
kanz_risk_recompute_duration_seconds_bucket{le="0.025"} 98
kanz_risk_recompute_duration_seconds_bucket{le="+Inf"} 100
kanz_risk_recompute_duration_seconds_sum 0.71
kanz_risk_recompute_duration_seconds_count 100
`

func parse(t *testing.T, text string) promscrape.Scrape {
	t.Helper()
	s, err := promscrape.Parse(bufio.NewScanner(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("promscrape.Parse: %v", err)
	}
	return s
}

func TestReadEngineTakesEveryCapacitySeries(t *testing.T) {
	e := readEngine(parse(t, engineScrapeText))
	if e.goroutines != 87 {
		t.Errorf("goroutines = %v, want 87", e.goroutines)
	}
	if !e.rssKnown || e.rssBytes != 268435456 {
		t.Errorf("rss = %v (known=%v), want 268435456", e.rssBytes, e.rssKnown)
	}
	if !e.inflightOK || e.inflight != 4 {
		t.Errorf("inflight = %v (ok=%v), want 4", e.inflight, e.inflightOK)
	}
	if !e.queueDepthK || e.queueDepth != 132 {
		t.Errorf("queue depth = %v (ok=%v), want 132", e.queueDepth, e.queueDepthK)
	}
	if got := e.buckets[0.005]; got != 50 {
		t.Errorf("le=0.005 bucket = %v, want 50", got)
	}
	if got := e.buckets[math.Inf(1)]; got != 100 {
		t.Errorf("le=+Inf bucket = %v, want 100 — an unparsed +Inf bound silently drops the "+
			"overflow bucket, and every quantile above the widest finite bound then reads as "+
			"that bound instead of as an overrun", got)
	}
}

// AN ABSENT SERIES IS NOT A ZERO, and this harness's whole output turns on it.
// A run against an engine that exports no width gauge — one predating #1050, or
// one whose collectors were moved behind a branch (#973, #963) — must report
// UNKNOWN, because a peak-of-0 quoted into a resources: block reads as measured.
func TestAnAbsentSeriesIsReportedUnknownNotZero(t *testing.T) {
	e := readEngine(parse(t, "go_goroutines 12\n"))
	if e.inflightOK || e.queueDepthK || e.rssKnown || e.goSysKnown {
		t.Fatalf("families the endpoint never exported were reported present: "+
			"inflight=%v queue=%v rss=%v gosys=%v", e.inflightOK, e.queueDepthK, e.rssKnown, e.goSysKnown)
	}

	c := &capacityReport{}
	c.polls, c.haveFirst = 3, true
	c.first, c.last = e, e
	c.peakGoroutines = 12
	out := c.render("http://engine/metrics", 1000)
	for _, want := range []string{
		"peak RSS:                   UNKNOWN",
		"peak recomputes in flight:  UNKNOWN",
		"peak recompute backlog:     UNKNOWN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q — it must not print a number it did not measure.\n%s", want, out)
		}
	}
	if strings.Contains(out, "peak RSS:                   0 B") {
		t.Error("the report printed a peak RSS of 0 B for a platform that does not export it")
	}
}

// A RUN THAT NEVER READ THE ENGINE MAKES NO CAPACITY CLAIM AT ALL.
func TestAnUnreadableEndpointRefusesToReport(t *testing.T) {
	c := &capacityReport{polls: 5, failed: 5}
	out := c.render("http://engine/metrics", 1000)
	if !strings.Contains(out, "UNKNOWN — the engine's metrics endpoint was never read successfully") {
		t.Errorf("a run with no successful scrape still made a capacity claim:\n%s", out)
	}
	if !strings.Contains(out, "The publish numbers above") {
		t.Errorf("the report did not say which half of the run is still valid:\n%s", out)
	}
}

// Quantiles answer at BUCKET RESOLUTION. Interpolating inside a bucket that spans
// 1s to 2.5s would invent digits the exporter never had, and the number would be
// quoted as a measurement.
func TestHistogramQuantileIsBucketResolution(t *testing.T) {
	b, n := deltaBuckets(emptySample(), readEngine(parse(t, engineScrapeText)))
	if n != 100 {
		t.Fatalf("observation delta = %v, want 100", n)
	}
	for _, tc := range []struct {
		q    float64
		want float64
	}{
		{0.50, 0.005}, // 50th of 100 lands exactly on the le=0.005 cumulative count
		{0.95, 0.025},
		// The 99th observation is NOT covered by le=0.025 (cumulative 98), so it
		// falls in the overflow bucket and must report as an overrun rather than
		// as the widest finite bound — which is the direction that would flatter.
		{0.99, math.Inf(1)},
	} {
		got, ok := histogramQuantile(b, n, tc.q)
		if !ok || got != tc.want {
			t.Errorf("q%v = %v (ok=%v), want %v", tc.q, got, ok, tc.want)
		}
	}
	// Everything slower than the widest finite bucket must report as an overrun,
	// never as that bucket's bound.
	over := map[float64]float64{0.001: 0, math.Inf(1): 10}
	if got, ok := histogramQuantile(over, 10, 0.99); !ok || !math.IsInf(got, 1) {
		t.Errorf("an all-overflow histogram gave q99 = %v (ok=%v), want +Inf", got, ok)
	}
	if _, ok := histogramQuantile(nil, 0, 0.99); ok {
		t.Error("a quantile was reported over an empty histogram")
	}
}

// emptySample is the zeroed opening scrape the delta is taken against.
func emptySample() engineSample {
	return engineSample{buckets: map[float64]float64{}}
}
