package balancerecon

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/execution"
)

// UnknownMetricName is the counter both venue adapters export for assets a
// reconciliation pass could not check (#1063). Named as a constant for the
// reason MetricName is: the observability guard and the alert rule refer to one
// spelling.
const UnknownMetricName = "kanz_venue_balance_unknown_total"

// DefaultUnknownLogEvery bounds how often ONE reason may write an ERROR line.
//
// A ceiling is required rather than tidy. The unknown is decided per ASSET and
// the exchange answers with every asset it lists — Binance's GET /api/v3/account
// returns hundreds — so a line per asset writes hundreds of identical lines per
// reconciliation pass, once a minute, for as long as the cash spine is down.
// That buries the rest of the log on the day it matters, which is the same
// argument MarkTickPublisher makes for rate-limiting its WARN.
//
// FIFTEEN MINUTES BECAUSE THAT IS DefaultMaxAge, the freshness bound whose
// expiry produces the stale reason in the first place: one line per window is
// one line per opportunity for the state to have changed on its own.
const DefaultUnknownLogEvery = DefaultMaxAge

// NewUnknownCounter builds the per-venue counter for unchecked assets, SEEDED AT
// ZERO for every reason before it is returned.
//
// THE SEEDING IS IN THE CONSTRUCTOR, NOT IN THE COMPOSITION ROOT, and that is
// the point of this function existing. An un-incremented CounterVec label
// exports no series at all, so a reason nobody has hit yet is indistinguishable
// from a metric nobody wired, and an alert over it is silent in exactly the
// state it detects — twice shipped in this estate (#973, #963) and caught both
// times only by running the binary. Seeding from execution.BalanceUnknownReasons
// here means a fourth reason added to that slice arrives seeded on both venues
// without either main being edited, rather than seeded on whichever one somebody
// remembered.
func NewUnknownCounter(venue string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: UnknownMetricName,
		Help: "Assets a balance reconciliation pass could not check, by reason. " +
			"Non-zero means this adapter is NOT comparing those assets against the exchange: " +
			"the pass completes and reports no break, so a mis-booked position on them would " +
			"pass unnoticed by the layer that exists to catch it.",
		ConstLabels: prometheus.Labels{"venue": venue},
	}, []string{"reason"})
	for _, reason := range execution.BalanceUnknownReasons {
		c.WithLabelValues(reason)
	}
	return c
}

// unknownReasonSlots is how many rate-limit slots the observer keeps — one per
// reason. Asserted against execution.BalanceUnknownReasons in this package's
// tests, so a reason added to the shared set cannot quietly lose its slot.
const unknownReasonSlots = 3

// UnknownObserver is the WorkerDeps.OnUnknownBalance seam: it counts every
// unchecked asset and says so, at a bounded log rate.
//
// TWO SIGNALS, NOT ONE, and the split is the same as MarkTickPublisher's. The
// COUNTER moves on every asset, because that is the number an alert and a
// dashboard need — "how much of this account is going unchecked" is a volume
// question. The ERROR LOG is rate-limited per reason, because it is a diagnosis
// and hundreds of identical lines a minute is not one.
//
// THE ASSET IS NOT A LABEL, deliberately. The exchange lists every asset it
// supports, not merely the ones this account holds, so a per-asset series is
// bounded by the venue's listings rather than by anything this platform
// configured — hundreds of permanent series per venue, on a counter whose whole
// purpose is to be read as one number. The asset that triggered each window's
// line is in the log, which is where the operator chasing it reads it.
type UnknownObserver struct {
	counter  *prometheus.CounterVec
	logger   *slog.Logger
	venue    string
	logEvery time.Duration
	now      func() time.Time

	mu sync.Mutex
	// last and suppressed are indexed by position in
	// execution.BalanceUnknownReasons — ARRAYS rather than a map keyed by reason,
	// so the state is bounded by the type itself and there is no eviction
	// question to answer.
	last       [unknownReasonSlots]time.Time
	suppressed [unknownReasonSlots]int
}

// unknownReasonIndex fixes each reason's slot.
var unknownReasonIndex = map[string]int{
	execution.BalanceUnknownNeverAnnounced: 0,
	execution.BalanceUnknownStale:          1,
	execution.BalanceUnknownUnattributed:   2,
}

// UnknownOption customizes an UnknownObserver.
type UnknownOption func(*UnknownObserver)

// WithUnknownLogEvery overrides DefaultUnknownLogEvery. Non-positive ⇒ every
// unchecked asset writes a line, which is only right in a test.
func WithUnknownLogEvery(d time.Duration) UnknownOption {
	return func(o *UnknownObserver) { o.logEvery = d }
}

// WithUnknownClock injects the clock (tests).
func WithUnknownClock(now func() time.Time) UnknownOption {
	return func(o *UnknownObserver) {
		if now != nil {
			o.now = now
		}
	}
}

// NewUnknownObserver binds the counter and the logger for one venue.
//
// A nil counter or a nil logger degrades this rather than panicking it: it runs
// inside a background reconciliation goroutine, and losing the process is a
// worse outcome than losing one of the two signals.
func NewUnknownObserver(counter *prometheus.CounterVec, logger *slog.Logger, venue string, opts ...UnknownOption) *UnknownObserver {
	if logger == nil {
		logger = slog.Default()
	}
	o := &UnknownObserver{
		counter: counter, logger: logger, venue: venue,
		logEvery: DefaultUnknownLogEvery, now: time.Now,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Observe records one asset this pass could not check. It is the value a
// composition root assigns to execution.WorkerDeps.OnUnknownBalance.
//
// IT RETURNS NOTHING, for the reason MarkTickPublisher.PublishTrade does: the
// reconciler has no better answer to an asset it cannot check than this one, so
// there is nothing to hand back and nothing left to discard.
func (o *UnknownObserver) Observe(asset, reason string) {
	reason = execution.NamedBalanceUnknown(reason)
	if o.counter != nil {
		o.counter.WithLabelValues(reason).Inc()
	}
	slot, known := unknownReasonIndex[reason]
	if !known {
		// NamedBalanceUnknown guarantees membership, so this arm fires only if a
		// reason joined the shared set without joining the index. It logs EVERY
		// occurrence: a silent skip here would restore exactly the silence this
		// seam exists to remove, on the reason nobody has thought about yet.
		o.log(asset, reason, 0)
		return
	}
	o.mu.Lock()
	now := o.now()
	due := o.logEvery <= 0 || o.last[slot].IsZero() || now.Sub(o.last[slot]) >= o.logEvery
	var suppressed int
	if due {
		o.last[slot] = now
		suppressed = o.suppressed[slot]
		o.suppressed[slot] = 0
	} else {
		o.suppressed[slot]++
	}
	o.mu.Unlock()
	if due {
		o.log(asset, reason, suppressed)
	}
}

// log writes the diagnosis. It names the CONSEQUENCE rather than the condition:
// an operator reading "balance unknown" has no way to know that the clean-looking
// reconciliation they are also reading is the symptom.
func (o *UnknownObserver) log(asset, reason string, suppressed int) {
	o.logger.Error("BALANCE RECONCILIATION COULD NOT CHECK THIS ASSET — the pass completed and "+
		"reported no break, which is exactly what a clean comparison looks like. Nothing is "+
		"comparing this account's books against the exchange for it, so a fill that was missed, "+
		"double-counted or posted to the wrong account would not be caught by the layer that "+
		"exists to catch it",
		"venue", o.venue, "asset", asset, "reason", reason,
		"suppressed_since_last_line", suppressed,
		"metric", UnknownMetricName,
		"next_step", "check "+MetricName+" and whether "+Subject+" is reaching this adapter")
}
