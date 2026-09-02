package main

import (
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/treasury"
)

// HOW MUCH CASH THE ESTATE IS SITTING ON, AND HOW MUCH OF THAT IT CAN EVEN SEE
// (#963).
//
// Uninvested cash is a permanent performance drag that breaches no mandate,
// trips no risk limit and produces no reconciliation break. Nothing in this
// platform would ever report it, so the first question — is it worth building a
// sweep engine at all — had no number behind it.
//
// # Two series, and the second is the one that matters today
//
// The histogram is the sizing instrument: the distribution of idle-cash share
// across every book the post-trade monitor holds. The counter is the honest
// companion, and with #588 open it will dominate — most books cannot have a drag
// stated at all, because accounting folds six kinds of journal entry and two have
// no producer anywhere, so their cash figure is short every dividend and coupon
// ever paid. `sum(kanz_treasury_drag_unmeasurable_total) by (reason)` turns that
// from an open ticket into a count of portfolios.
//
// # There is deliberately NO alert on the idle LEVEL
//
// An alert needs a threshold and a threshold is a policy — a target buffer, a
// maximum idle balance — and #963 is explicit that the cash policy belongs where
// mandates are held, because it is a mandate-shaped constraint. Picking a number
// here would be inventing that policy in a metrics file, in the one service that
// must not hold a second copy of a constraint. So the histogram is exported for
// reading and sized, and the only rule written is the one that says the estate
// cannot measure itself. When the policy lands, the alert becomes "this book is
// outside the buffer its mandate declares", which is a different rule entirely.
//
// # No portfolio label
//
// #963 proposed {portfolio,currency}. Portfolio is unbounded cardinality and,
// worse, a disclosure surface: tenant isolation here is deny-by-default and
// covers DISCOVERY, so one tenant's portfolio identifiers must not be legible
// from an estate-wide scrape. The distribution answers the sizing question
// without them, and the per-portfolio detail goes to the log, warn-once, exactly
// as PreTradeGate.noteUnaccounted reports the same #588 gap on the order path.

// idleCashShare is the distribution of idle cash as a share of equity.
//
// BUCKETS CHOSEN AS FUND POSTURES, not as round numbers. Below 1% is fully
// invested; 1-5% is an ordinary operating buffer; 5-10% is a deliberate defensive
// position or an oversight; above 10% is cash a treasury desk would want to
// explain. The point of the instrument is to say which of those the estate is in.
var idleCashShare = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name: "kanz_treasury_idle_cash_share",
	Help: "Uninvested cash as a share of equity, per portfolio, over books whose cash AND NAV are " +
		"both vouched for. A book that cannot be measured is NOT recorded here — it is counted " +
		"in kanz_treasury_drag_unmeasurable_total, because a refusal observed as 0.0 would " +
		"report a portfolio as holding no idle cash (#963).",
	Buckets: []float64{0.001, 0.005, 0.01, 0.02, 0.05, 0.10, 0.20, 0.50},
})

// dragUnmeasurable counts books whose idle cash cannot be stated, by reason.
//
// SEEDED FROM treasury.Reasons(), not from a list written here. A new refusal
// reason would otherwise ship with no series behind it and become invisible —
// the defect #806 and #803 both were, one hand-maintained enumeration each.
var dragUnmeasurable = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kanz_treasury_drag_unmeasurable_total",
	Help: "Book evaluations where uninvested cash could not be stated as a share of equity, by " +
		"reason. cash_unvouched is #588 — accounting folds corporate actions and accruals and " +
		"nothing produces either — and nav_not_equity is #780's gross-positions proxy (#963).",
}, []string{"reason"})

// registerTreasuryMetrics registers the cash-drag series and seeds the reasons.
//
// REGISTERED UNCONDITIONALLY, so an estate that can measure nothing reads as a
// rising counter rather than an absent series. That is the entire failure this
// measurement exists to end: cash drag currently produces no signal of any kind,
// and a metric that only appears once something can be measured would reproduce
// the silence one layer up (#622).
func registerTreasuryMetrics(r prometheus.Registerer) {
	r.MustRegister(idleCashShare, dragUnmeasurable)
	for _, reason := range treasury.Reasons() {
		dragUnmeasurable.WithLabelValues(string(reason))
	}
}

// cashDragObserver reports one book's drag to the metrics and, once per
// portfolio, to the log.
//
// THE HISTOGRAM IS ONLY EVER FED A MEASURED DRAG. treasury.Drag.Percent returns
// 0 on a refusal and documents that the caller must not record it; observing that
// would put every unmeasurable book into the lowest bucket and render an estate
// that can see nothing as an estate that is fully invested — which is both wrong
// and the flattering direction, so nothing downstream would question it.
//
// THE PER-PORTFOLIO DETAIL IS LOGGED ONCE, matching PreTradeGate.noteUnaccounted:
// loud enough for an operator to find the portfolio and the missing feed, quiet
// enough that a monitor sweeping every book on an interval does not drown the log
// with the same finding forever.
func cashDragObserver(logger *slog.Logger, seen func(key string) bool) func(string, string, treasury.Drag) {
	return func(tenantID, portfolioID string, d treasury.Drag) {
		if d.Measured() {
			idleCashShare.Observe(d.Percent())
			return
		}
		dragUnmeasurable.WithLabelValues(string(d.Reason)).Inc()
		if logger == nil || seen == nil || !seen(string(d.Reason)+":"+tenantID+":"+portfolioID) {
			return
		}
		logger.Warn("CANNOT MEASURE this portfolio's idle cash",
			"tenant_id", tenantID,
			"portfolio_id", portfolioID,
			"reason", string(d.Reason),
			"detail", d.Detail,
			"consequence", "uninvested cash is a permanent drag that breaches no mandate and trips "+
				"no risk limit, so this portfolio's is reported by nothing at all (#963)")
	}
}

// onceSet remembers keys already reported, so a per-portfolio finding is logged
// once rather than on every sweep.
//
// UNBOUNDED BY DESIGN, AND BOUNDED BY WHAT IT KEYS ON. It grows with (reason,
// tenant, portfolio) triples, which is the estate's portfolio count times a
// closed set of six reasons — the same shape and the same argument as
// PreTradeGate's warn-once map. A cache with eviction here would re-log the same
// finding forever after each eviction, which is the noise this exists to avoid.
type onceSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newOnceSet() *onceSet { return &onceSet{seen: map[string]struct{}{}} }

// first reports whether this key is being seen for the first time.
//
// The monitor's sweep and its bus handlers run concurrently, so this is locked:
// an unsynchronised map here is a data race in a process that is otherwise
// careful about exactly that, and -race does not run on the usual dev box.
func (o *onceSet) first(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.seen[key]; ok {
		return false
	}
	o.seen[key] = struct{}{}
	return true
}
