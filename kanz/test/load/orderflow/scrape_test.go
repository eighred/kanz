package main

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// The exposition subset this harness parses, exercised against the shapes the
// OMS actually exports. A parser that quietly returned an empty scrape would make
// every refusal fire for the wrong reason — safe, but it would send whoever read
// it to the wrong process.

const omsSample = `# HELP kanz_oms_live_venue_adapters Out-of-process venue.v1 adapters.
# TYPE kanz_oms_live_venue_adapters gauge
kanz_oms_live_venue_adapters 0
kanz_oms_simulated_venues 1
kanz_bus_pending_messages{group="oms",subject="order.order.amend"} 3
kanz_bus_pending_messages{group="oms",subject="order.order.submit"} 158
kanz_bus_pending_messages{group="oms-tca",subject="order.order.filled"} 7
kanz_compliance_ungoverned_orders_total 10
`

func TestParseScrapeReadsLabelledAndUnlabelledSeries(t *testing.T) {
	s, err := parseScrape(bufio.NewScanner(strings.NewReader(omsSample)))
	if err != nil {
		t.Fatalf("parseScrape: %v", err)
	}
	if v, ok := s.value("kanz_oms_simulated_venues"); !ok || v != 1 {
		t.Errorf("kanz_oms_simulated_venues = %v (present=%v), want 1", v, ok)
	}
	if _, ok := s.value("kanz_oms_venue_margin_uncovered_total"); ok {
		t.Error("a family the endpoint never exported was reported as present — absent and zero must " +
			"stay distinguishable, because every refusal in preflight.go turns on it")
	}
	got, matched, present := s.sum("kanz_bus_pending_messages",
		`group="oms"`, `subject="order.order.submit"`)
	if !present || matched != 1 || got != 158 {
		t.Errorf("the order-command backlog = %v (matched=%d present=%v), want 158 from exactly one "+
			"series — a filter that also caught the amend or the -tca group would report a backlog "+
			"that is not the one the write path queues on", got, matched, present)
	}
}

// pendingCommands is the write path's saturation signal, and its "could not be
// read" answer must not look like a backlog of zero.
func TestPendingCommandsSeparatesUnreadFromZero(t *testing.T) {
	s, _ := parseScrape(bufio.NewScanner(strings.NewReader(omsSample)))
	if v, ok := pendingCommands(s, "oms", "order.order.submit"); !ok || v != 158 {
		t.Errorf("pendingCommands = %v (ok=%v), want 158", v, ok)
	}
	if _, ok := pendingCommands(s, "some-other-group", "order.order.submit"); ok {
		t.Error("a group with no series was reported as readable; the run would then print a " +
			"reassuring 0 for a signal it never saw")
	}
}

// A BODY THIS PARSER CANNOT READ IS AN ERROR. An HTML error page, a proxy's login
// form or a truncated response would otherwise parse to an empty scrape, which
// every caller reads as "the metric is absent".
func TestUnreadableMetricsBodiesAreErrors(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"an HTML error page", "<html><body>502 Bad Gateway</body></html>\n"},
		{"a value that is not a number", "kanz_oms_simulated_venues one\n"},
		{"nothing but comments", "# HELP x y\n# TYPE x gauge\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseScrape(bufio.NewScanner(strings.NewReader(tt.text))); err == nil {
				t.Fatal("parsed without error, so the caller would see an empty scrape and report " +
					"every metric as absent")
			}
		})
	}
}

// A COUNTER THIS HARNESS COULD NOT READ IS UNKNOWN, NEVER ZERO. Reporting "no
// control degraded" on the strength of a metric that was not there is the exact
// collapse the whole watch list exists to prevent.
func TestControlDeltasReportAnUnreadableCounterAsUnknown(t *testing.T) {
	before := scrapeOf(t, "kanz_compliance_ungoverned_orders_total 4\nkanz_oms_claim_timeouts_total 0\n")
	after := scrapeOf(t, "kanz_compliance_ungoverned_orders_total 9\n")

	var sawUnknown, sawDelta bool
	for _, d := range controlDeltas(before, after) {
		switch d.metric {
		case "kanz_compliance_ungoverned_orders_total":
			sawDelta = true
			if d.unknown || d.delta != 5 {
				t.Errorf("ungoverned delta = %v (unknown=%v), want 5", d.delta, d.unknown)
			}
			if !d.invalidates {
				t.Error("an ungoverned-order delta must INVALIDATE the run: the pre-trade gate " +
					"returns at its first branch for those orders, so the throughput measured " +
					"across them is a short circuit and not the gate")
			}
		case "kanz_oms_claim_timeouts_total":
			sawUnknown = true
			if !d.unknown {
				t.Error("a counter missing from the closing scrape was reported as a real delta")
			}
		}
	}
	if !sawDelta || !sawUnknown {
		t.Fatalf("the watch list no longer covers the two metrics this test names (delta=%v unknown=%v)",
			sawDelta, sawUnknown)
	}
}

// Percentiles are nearest-rank, matching k6 and every budget in test/load.
func TestPercentilesAreNearestRank(t *testing.T) {
	d := make([]time.Duration, 100)
	for i := range d {
		d[i] = time.Duration(i+1) * time.Millisecond
	}
	p50, p95, p99 := percentiles(d)
	if p50 != 50*time.Millisecond || p95 != 95*time.Millisecond || p99 != 99*time.Millisecond {
		t.Errorf("p50/p95/p99 = %s/%s/%s, want 50ms/95ms/99ms", p50, p95, p99)
	}
	if a, b, c := percentiles(nil); a != 0 || b != 0 || c != 0 {
		t.Errorf("percentiles of nothing = %s/%s/%s, want zeroes", a, b, c)
	}
}
