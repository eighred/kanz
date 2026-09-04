package main

// THE RESIDENT BOOK WAS UNBOUNDED (#809).
//
// #988 bounded the BOOT with a fold checkpoint and left the HEAP: the projection
// appends an execution per fill, an order revision per status transition (each
// carrying a full cloned OrderState) and a fill id per fill, and nothing ever
// removed one. A pod holding a busy fund grows for its whole life until it is
// OOM-killed — and the failure is worse than the memory, because the operator
// interface disappears at the moment an incident makes somebody want it.
//
// WHAT THIS LOOP DOES NOT DO is shorten the record. tv_facts is untouched;
// retention is in RAM only, what leaves is folded into a per-account baseline on
// the way out, and every position, average cost and realized-P&L figure the
// Broker API serves is identical with it on and off. See
// services/tv-sync/internal/projection/retention.go for why that is exact rather
// than approximate.

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

// residentEvicted counts what retention has dropped, by collection.
//
// EVERY LABEL VALUE IS SEEDED AT REGISTRATION, because a CounterVec label that is
// never incremented exports NO SERIES and an alert written over it evaluates to
// nothing in exactly the state it was written for (#973, #963, #983, #1005). A
// pod whose fills are all inside the window legitimately evicts no executions;
// that must read as a zero, not as an absent metric.
var residentEvicted = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kanz_tvsync_retention_evicted_total",
	Help: "Folded history dropped from memory by retention, by collection. The durable fact log is " +
		"untouched and every position and P&L figure is unchanged — this is resident set only (#809).",
}, []string{"collection"})

// residentHeld is the number that answers #809's "Verified when".
//
// THE ISSUE'S TEST IS "RESIDENT SET FLAT AGAINST TOTAL HISTORY", and this is the
// only place it can be observed on a running estate: a line that keeps climbing
// while the fund keeps trading is the leak still present, whatever the code says.
var residentHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "kanz_tvsync_resident_history",
	Help: "Folded history currently held in memory, by collection. Flat against a growing fund is " +
		"the property #809 asks for; a line that tracks lifetime volume is the leak (#809).",
}, []string{"collection"})

// retentionSeconds is the configured window, exported so the posture is readable
// rather than inferred from the eviction rate. Zero would mean unbounded —
// config.Load refuses it, and this gauge is how a deployment that somehow reached
// it says so.
var retentionSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_tvsync_retention_seconds",
	Help: "The configured resident-history window in seconds. Zero means unbounded, which is the " +
		"pre-#809 leak; config.Load refuses to start that way (#809).",
})

const (
	collExecutions = "executions"
	collOrders     = "orders"
	collFills      = "fills"
)

// registerRetentionMetrics registers the resident-set series.
//
// CALLED BEFORE THE POSTGRES DIAL, outside every branch that depends on the
// database being reachable — the wiring defect #973, #963 and #983 each shipped
// once, where a collector registered inside `if pool != nil` exported nothing in
// precisely the degraded posture its alert was written for.
func registerRetentionMetrics(r prometheus.Registerer, window time.Duration) {
	r.MustRegister(residentEvicted, residentHeld, retentionSeconds)
	for _, c := range []string{collExecutions, collOrders, collFills} {
		residentEvicted.WithLabelValues(c)
		residentHeld.WithLabelValues(c).Set(0)
	}
	retentionSeconds.Set(window.Seconds())
}

// runRetention drops folded history older than the window, on an interval, until
// ctx is done.
//
// IT DOES NOT EVICT ON SHUTDOWN, and the asymmetry with runCheckpoints is
// deliberate: a checkpoint on the way out saves the NEXT boot work, while an
// eviction on the way out saves a process that is about to exit nothing at all
// and would move the horizon in the last checkpoint for no gain.
//
// A PASS THAT DROPS NOTHING IS NORMAL AND STILL PUBLISHES. A fund whose whole
// history is inside the window evicts zero, and the resident gauges are how that
// is told apart from a loop that is not running — which is the failure this whole
// change exists to prevent coming back silently.
func runRetention(ctx context.Context, proj *projection.Projection, every time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := proj.Evict(time.Now())
			residentEvicted.WithLabelValues(collExecutions).Add(float64(st.Executions))
			residentEvicted.WithLabelValues(collOrders).Add(float64(st.Orders))
			residentEvicted.WithLabelValues(collFills).Add(float64(st.Fills))
			held := proj.Resident()
			residentHeld.WithLabelValues(collExecutions).Set(float64(held.Executions))
			residentHeld.WithLabelValues(collOrders).Set(float64(held.Orders))
			residentHeld.WithLabelValues(collFills).Set(float64(held.Fills))
			if st.Executions > 0 || st.Orders > 0 {
				logger.Debug("retention pass", "evicted_executions", st.Executions,
					"evicted_orders", st.Orders, "evicted_fills", st.Fills,
					"resident_executions", held.Executions, "resident_orders", held.Orders)
			}
		}
	}
}

// retentionSweep is how often the window is applied, NOT how long history is kept.
//
// The window itself is TV_SYNC_RETENTION and is a correctness-bearing number; the
// sweep is only how promptly memory is returned after a fact ages out, so a
// minute is chosen the way the checkpoint interval is — small enough that the
// overshoot between passes is a minute of one fund's volume, large enough that a
// pass (which reallocates each account's execution slice) is not on the fold's
// critical path.
const retentionSweep = time.Minute
