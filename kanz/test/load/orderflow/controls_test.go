package main

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/test/load/internal/promscrape"
)

// The saturation and control signals this harness reads out of a scrape. The
// PARSER moved to test/load/internal/promscrape when test/load/ingest became its
// second consumer (#1050); what stays here is the orderflow-specific reading of
// it, which is where the write path's own judgements live.

const omsBacklogSample = `kanz_bus_pending_messages{group="oms",subject="order.order.amend"} 3
kanz_bus_pending_messages{group="oms",subject="order.order.submit"} 158
kanz_bus_pending_messages{group="oms-tca",subject="order.order.filled"} 7
`

// pendingCommands is the write path's saturation signal, and its "could not be
// read" answer must not look like a backlog of zero.
func TestPendingCommandsSeparatesUnreadFromZero(t *testing.T) {
	s, _ := promscrape.Parse(bufio.NewScanner(strings.NewReader(omsBacklogSample)))
	if v, ok := pendingCommands(s, "oms", "order.order.submit"); !ok || v != 158 {
		t.Errorf("pendingCommands = %v (ok=%v), want 158", v, ok)
	}
	if _, ok := pendingCommands(s, "some-other-group", "order.order.submit"); ok {
		t.Error("a group with no series was reported as readable; the run would then print a " +
			"reassuring 0 for a signal it never saw")
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
