// Package balancerecon makes a venue adapter's balance-reconciliation posture
// OBSERVABLE — including, and especially, when it is not running at all (#418).
//
// The exchange is the authority on what the fund owns. Everything kanz reports —
// NAV, exposure, every limit checked against them — is a projection of FACTs it
// derived from its own fills. Comparing those books against the exchange's
// actual balances is the only thing that can notice a fill that was missed,
// double-counted, or posted to the wrong account.
//
// THAT COMPARISON HAS NEVER RUN, AND ITS SILENCE LOOKS EXACTLY LIKE SUCCESS.
// execution.WorkerDeps.Balances was omitted by both venue composition roots, so
// reconcileBalances short-circuits on nil and returns nil. The FACT it would
// publish, accounting.balance.reconciled, is emitted ONLY ON A DISCREPANCY — so
// an operator filtering on that subject sees zero events whether the books agree
// or no comparison has ever happened. Zero reads as healthy.
//
// This package exists so those two states are told apart, and it lives here
// rather than in each main because it was otherwise fifteen lines of identical
// prose in two composition roots — the copied-helper shape AGENTS.md names as
// how a fix stops spreading.
package balancerecon

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/execution"
)

// MetricName is the gauge the adapters export. Named as a constant so the
// observability guard and any dashboard refer to one spelling.
const MetricName = "kanz_venue_balance_reconciliation_configured"

// NewGauge builds the per-venue posture gauge. The caller registers it on the
// service registry, as it does for every other adapter metric.
//
// A GAUGE AND NOT A LOG LINE, because the question is asked during an incident
// and asked of a dashboard: is this silence "the books agree" or "nothing has
// ever checked"? A log line answers that only for whoever thinks to grep. This
// mirrors kanz_venue_orderview_durable, which exists for the same reason one
// degraded posture over.
func NewGauge(venue string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Name: MetricName,
		Help: "1 if the venue adapter can compare Kanz's balances against the exchange's, 0 if not. " +
			"0 means accounting.balance.reconciled is silent because NO COMPARISON RUNS, " +
			"not because the books agree.",
		ConstLabels: prometheus.Labels{"venue": venue},
	})
}

// Announce records the posture and returns balances unchanged, so a composition
// root wires it in the same expression that fills the field:
//
//	Balances: balancerecon.Announce(g, logger, "binance", nil),
//
// Returning the seam rather than taking a pointer is what stops the gauge and
// the wiring drifting apart: there is no way to set one without the other,
// because the value the worker receives is this function's return.
//
// nil is not an error and does not refuse startup. A venue adapter that cannot
// reconcile balances can still route and fill orders correctly, and refusing to
// trade over a missing REPORTING seam would be a self-inflicted outage. It must
// simply never be quiet about it.
func Announce(g prometheus.Gauge, logger *slog.Logger, venue string, balances execution.ExpectedBalances) execution.ExpectedBalances {
	if balances != nil {
		if g != nil {
			g.Set(1)
		}
		return balances
	}
	if g != nil {
		g.Set(0)
	}
	if logger != nil {
		logger.Warn("BALANCE RECONCILIATION IS NOT RUNNING — this adapter has never compared kanz's "+
			"books against the exchange's actual balances, and cannot until something implements "+
			"execution.ExpectedBalances (#418). Zero events on accounting.balance.reconciled means "+
			"NO COMPARISON HAS RUN, not that the books agree.",
			"venue", venue,
			"metric", MetricName,
		)
	}
	return nil
}
