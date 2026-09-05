package main

import (
	"sort"

	"github.com/eighred/kanz/test/load/internal/promscrape"
)

// controlSignal is one counter this harness watches ACROSS the run, with the
// sentence a non-zero delta means. The set is the point: a write-path load run
// that reported only latency and error rate would miss every failure mode #865
// names, because none of them is an error the client sees.
type controlSignal struct {
	metric string
	// invalidates is true when a non-zero delta means the RUN MEASURED SOMETHING
	// ELSE, as opposed to the run having found a real degradation. The two are
	// different verdicts: one says the platform degraded, the other says the
	// number must not be quoted at all.
	invalidates bool
	meaning     string
}

// watchedControls is what a write-path run reports deltas for.
//
// DERIVED FROM NOTHING — IT IS A HAND-WRITTEN LIST, AND THAT IS A KNOWN LIMIT.
// The estate's guidance is to derive such a set from its source of truth where
// one exists; there is no registry of "counters that mean a control degraded",
// and inventing one to serve a load harness would be the harness redefining the
// platform. What this list must not become is silently stale, so
// scrape_test.go asserts every name here is exported by the OMS's own
// registry — a metric renamed out from under this file fails a test rather than
// reporting a clean run forever.
var watchedControls = []controlSignal{
	{
		metric:      "kanz_compliance_ungoverned_orders_total",
		invalidates: true,
		meaning: "orders were admitted for a portfolio NO MANDATE GOVERNS. The pre-trade gate " +
			"returns at its first branch for those — no rule engine, no book projection, no " +
			"margin resolution — so a throughput number taken across them measures a short " +
			"circuit and not the gate. Put the portfolio under mandate (kanz-mandate) and re-run",
	},
	{
		metric:      "kanz_compliance_unreadable_mandate_orders_total",
		invalidates: true,
		meaning: "orders were refused because the portfolio's published mandate could not be " +
			"applied. Those never reach the rule engine either, so the run measured a refusal path",
	},
	{
		metric:      "kanz_compliance_decisions_dropped_total",
		invalidates: false,
		meaning: "pre-trade compliance decisions were NOT recorded. Each one is a decision about " +
			"whether capital moves with no audit record behind it — the recorder's queue could " +
			"not keep up with admission",
	},
	{
		metric:      "kanz_oms_claim_timeouts_total",
		invalidates: false,
		meaning: "the per-order claim (orderlock) timed out: an operator's cancel or amend was " +
			"parked in the DLQ while the order was still working at a venue. This is the " +
			"head-of-line control giving up under contention",
	},
	{
		metric:      "kanz_oms_orders_quarantined_total",
		invalidates: false,
		meaning: "orders were frozen because the platform could not establish what the venue did " +
			"with them. Each one is a position whose true size nobody knows",
	},
	{
		metric:      "kanz_oms_outbox_publish_failures_total",
		invalidates: false,
		meaning: "order FACTs were committed and could not be published. The estate does not know " +
			"about orders the store has admitted",
	},
	{
		metric:      "kanz_oms_shared_collateral_orders_total",
		invalidates: false,
		meaning: "orders executed against an exchange account not bound to their portfolio. " +
			"Expected on a rig with no OMS_VENUE_ACCOUNTS, and reported so the number is never " +
			"read as a clean segregation result",
	},
}

// controlDeltas compares two scrapes over watchedControls. A metric absent from
// either scrape is reported as a delta of NaN with `unknown` set, never as zero:
// this harness may not report "no control degraded" on the strength of a counter
// it could not read.
type controlDelta struct {
	controlSignal
	delta   float64
	unknown bool
}

func controlDeltas(before, after promscrape.Scrape) []controlDelta {
	out := make([]controlDelta, 0, len(watchedControls))
	for _, c := range watchedControls {
		b, okB := before.Value(c.metric)
		a, okA := after.Value(c.metric)
		out = append(out, controlDelta{controlSignal: c, delta: a - b, unknown: !okA || !okB})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].metric < out[j].metric })
	return out
}

// pendingCommands reads the standing backlog of order COMMANDS for a consumer
// group: kanz_bus_pending_messages{group=<g>,subject=order.order.submit}.
//
// THIS IS THE WRITE PATH'S KNEE SIGNAL, and it is the same series that
// capacity-model.md already sizes KEDA against for the risk-engine — reused
// rather than re-invented, so a write-path capacity number and the autoscaling
// policy speak about one quantity. A backlog that grows across a stage and does not
// drain is the write-path equivalent of the read path's p99 breaking: the client
// still sees 202s, because the gateway accepted the command; the ADMISSION is
// what is falling behind.
func pendingCommands(s promscrape.Scrape, group, subject string) (float64, bool) {
	total, matched, present := s.Sum("kanz_bus_pending_messages",
		`group="`+group+`"`, `subject="`+subject+`"`)
	return total, present && matched > 0
}
