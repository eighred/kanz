package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/outbox"
)

// THE OUTBOX RELAY'S INSTRUMENTATION (#292), EXTRACTED SO THE THREE SIGNALS ARE
// ONE THING (#643).
//
// The relay itself is built by order.NewService, from the store's own queue and
// the emitter's own bus, so a composition holding a Service always holds a drain
// — there is no wiring step here that could be omitted. What the composition root
// owns is RUNNING it and MEASURING it, and the measuring half was split across
// ninety lines of runConsumers: two counters constructed before the service, a
// gauge registered after it, and nothing naming them as one instrument.
//
// There is no switch to turn the relay off, deliberately. A disabled relay is an
// OMS that admits orders, commits their ACCEPTED FACTs to a table and tells
// nobody — the exact invisible-order failure #238 was filed for, made permanent.
// "Nothing configured" and "checked, and fine" must not look the same, and the
// cheapest way to guarantee that is for the off position not to exist.
//
// # THE THREE SIGNALS ANSWER THREE DIFFERENT QUESTIONS
//
// A published counter says the relay is working. A failure counter says it is
// trying and being refused. NEITHER SEPARATES A DRAINED OUTBOX FROM A DEAD RELAY:
// both produce zero errors, zero publishes and a silent log. Only the age of the
// oldest unpublished record does, which is why it is a GaugeFunc — it has to be
// true when nothing is happening, which is exactly when it matters.

// buildOutboxRelayOptions registers the relay's two counters AND hands them to
// the relay, in one scope.
//
// # The shape is chosen by a guard, not by taste
//
// test/arch's TestEveryRegisteredMetricHasAWriter proves that something in the
// module writes every registered collector. It tracks a collector by the name it
// is BOUND to and stops tracking when that name is handed to a function — so the
// construction and the hand-off must share a scope and a name, or the guard
// silently stops covering the metric.
//
// Two shapes were tried and REJECTED ON EVIDENCE, each by mutating the wiring
// away and watching the guard stay green:
//
//   - a struct of counters (m.Published) — the guard does not follow selectors,
//     and widening it to do so let an unrelated `Published` field in
//     tools/natsrebuild vouch for this one;
//   - returning the counters for the caller to pass on — the binding here and
//     the binding there are different names, so the hand-off marked a name this
//     function's collector never had.
//
// Returning the OPTIONS keeps all three events — construct, register, hand off —
// on one identifier, which is the only shape the guard can verify. It is also
// better encapsulation than the original inline version: there is no way to take
// the counters without taking the wiring that writes them.
//
// THE LOCAL NAMES ARE DELIBERATELY NOT `published` AND `failures`. The guard is
// keyed by binding name across the whole module with no receiver-type
// resolution — its own documented limit — so a generic local is vouched for by
// any unrelated local of the same name that happens to be passed to a function
// somewhere. That was MEASURED, not assumed: with the short names, deleting the
// WithCounters line below left the guard green, and with these names it fails.
// Renaming a local is a cheap price for a guard that actually bites.
func buildOutboxRelayOptions(reg prometheus.Registerer, interval time.Duration) []outbox.RelayOption {
	outboxPublished := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_outbox_published_total",
		Help: "Order FACTs published from the transactional outbox. Every lifecycle FACT the OMS commits " +
			"transactionally is counted here exactly once per successful publish; a relay that has stopped " +
			"shows as a flat line while orders keep being admitted.",
	})
	outboxFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_outbox_publish_failures_total",
		Help: "Attempts to publish an outbox record that did not reach the broker. The record stays at the " +
			"head of its order's queue and every FACT behind it is held back — publishing past it would hand " +
			"consumers that order's history out of sequence. Nothing else surfaces this: the command that " +
			"enqueued the record was acked successfully long before.",
	})
	reg.MustRegister(outboxPublished, outboxFailures)
	return []outbox.RelayOption{
		outbox.WithInterval(interval),
		outbox.WithCounters(outboxPublished, outboxFailures),
	}
}

// depthGauge is the one signal that separates a drained outbox from a dead relay.
//
// A GaugeFunc, not a counter, and read from the LIVE relay: it must be true when
// nothing is happening. Zero means the outbox is drained. A value that keeps
// climbing means FACTs the estate depends on are sitting in Postgres — risk,
// compliance and the audit log are behind by that much, and no other series in
// this process says so.
type oldestPending interface {
	OldestPendingAge(ctx context.Context) float64
}

// announceOutboxDepth registers the age gauge over a live relay.
func announceOutboxDepth(ctx context.Context, relay oldestPending, reg prometheus.Registerer) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_outbox_oldest_pending_seconds",
		Help: "Age of the oldest order FACT committed to the outbox and not yet published. Zero means the " +
			"outbox is drained. A value that keeps climbing means FACTs the estate depends on are sitting in " +
			"Postgres: risk, compliance and the audit log are behind by that much.",
	}, func() float64 { return relay.OldestPendingAge(ctx) }))
}
