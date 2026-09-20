package execution

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrReconcileEvidence marks an observation that cannot be represented exactly.
// It must not turn into fabricated zero/rounded evidence or a clean comparison.
var ErrReconcileEvidence = errors.New("invalid reconciliation evidence")

const ReconcileErrorMetric = "kanz_venue_reconciliation_errors_total"

var reconcileLoops = [...]string{"reconcile", "healing", "unknown"}
var reconcileReasons = [...]string{"rate_limited", "invalid_evidence", "access_denied", "timeout", "other"}

// ReconcileErrorObserver counts every failed pass and bounds diagnostic logging
// independently for each loop/reason. No order IDs or raw exchange error strings
// enter labels or logs: upstream errors can contain credentials and payloads.
type ReconcileErrorObserver struct {
	counter *prometheus.CounterVec
	logger  *slog.Logger
	venue   string
	mu      sync.Mutex
	last    [3][5]time.Time
	now     func() time.Time
}

func NewReconcileErrorObserver(reg prometheus.Registerer, logger *slog.Logger, venue string) *ReconcileErrorObserver {
	if logger == nil {
		logger = slog.Default()
	}
	o := &ReconcileErrorObserver{logger: logger, venue: venue, now: time.Now}
	if reg != nil {
		o.counter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: ReconcileErrorMetric, Help: "Failed venue reconciliation passes, not repaired discrepancies.", ConstLabels: prometheus.Labels{"venue": venue}}, []string{"loop", "reason"})
		for _, loop := range reconcileLoops {
			for _, reason := range reconcileReasons {
				o.counter.WithLabelValues(loop, reason)
			}
		}
		reg.MustRegister(o.counter)
	}
	return o
}

func (o *ReconcileErrorObserver) Observe(ctx context.Context, loop string, err error) {
	if err == nil || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return
	}
	l := 2
	for i, name := range reconcileLoops {
		if loop == name {
			l = i
			break
		}
	}
	r := 4
	switch {
	case errors.Is(err, ErrRateLimited):
		r = 0
	case errors.Is(err, ErrReconcileEvidence):
		r = 1
	case errors.Is(err, ErrEgressDenied):
		r = 2
	case errors.Is(err, context.DeadlineExceeded):
		r = 3
	}
	if o.counter != nil {
		o.counter.WithLabelValues(reconcileLoops[l], reconcileReasons[r]).Inc()
	}
	o.mu.Lock()
	now := o.now()
	due := o.last[l][r].IsZero() || now.Sub(o.last[l][r]) >= time.Minute
	if due {
		o.last[l][r] = now
	}
	o.mu.Unlock()
	if due {
		var coverage *ReconcilePassError
		total, checked, failed := 0, 0, 0
		if errors.As(err, &coverage) {
			total, checked, failed = coverage.Total, coverage.Checked, coverage.Failed
		}
		o.logger.Error("venue reconciliation pass failed; absence of discrepancy events does not establish agreement with the venue", "venue", o.venue, "loop", reconcileLoops[l], "reason", reconcileReasons[r], "orders_total", total, "orders_checked", checked, "orders_failed", failed)
	}
}

// ErrCloseUnconfirmed means the venue has not confirmed a terminal close.
var ErrCloseUnconfirmed = errors.New("venue close remains unconfirmed")
