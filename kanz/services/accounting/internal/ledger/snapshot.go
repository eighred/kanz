package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Snapshotter writes the checkpoints MaterializeCurrent reads (#229).
//
// # Why a periodic job and not a fold-side checkpoint
//
// The obvious alternative is to checkpoint from the fold: after every Nth
// Append in the bus consumer, snapshot the book. Three things rule it out.
//
//  1. THE FOLD IS SHARDED. The consumer subscribes under a load-balanced
//     durable group, so fills for one portfolio are spread across replicas and
//     no replica holds a whole book in memory. A cheap in-process checkpoint
//     would therefore snapshot a PARTIAL book — a wrong NAV written into the
//     cache that reads it. Making it correct means re-reading the whole journal
//     from the store, at which point it is this job, just running on the
//     latency-critical ack path.
//  2. IT WOULD PUT A CACHE WRITE ON THE BOOK-OF-RECORD PATH. Handle returns an
//     error to nack and DLQ; a snapshot write that failed would either DLQ a
//     fill the journal already holds, or be swallowed — losing a book-of-record
//     entry, or losing the checkpoint silently. Neither is acceptable, and the
//     choice does not arise if the checkpoint is not on that path.
//  3. IT IS NOT WHERE THE WORK IS. A snapshot's cost is proportional to the
//     journal; a fill's is constant. Coupling them makes the append path
//     degrade with journal age, which is the exact failure #229 is about.
//
// So: one goroutine, in the service that already reads the book, doing a full
// fold per stale portfolio on a ticker. It costs one unbounded read per
// portfolio per interval, and it is what stops every NAV request from doing the
// same.
//
// # What happens when a snapshot write fails
//
// Nothing breaks and everything gets slower — which is why this job is
// instrumented rather than merely logged. A failed write leaves the previous
// checkpoint in place (or none), so MaterializeCurrent stays CORRECT and falls
// back to the full journal. That fallback is exactly the #229 defect, silently
// reinstated, and the whole reason a snapshot subsystem can rot unnoticed for
// as long as this one did. The three signals below make it visible:
//
//   - kanz_accounting_snapshot_writes_total{result} — ok vs error, per attempt.
//   - kanz_accounting_snapshot_last_success_timestamp_seconds — flat means the
//     job is dead or failing; alert on its age, not on the error counter, so a
//     job that stopped ticking altogether is caught too.
//   - kanz_accounting_snapshot_stale_portfolios — the queue depth at the last
//     pass. Persistently at the batch limit means the interval or the batch is
//     too small and the checkpoints will never catch up.
//
// A pass logs every failure at Error with the portfolio id, and returns the
// first error so a test can assert on it. Run keeps ticking afterwards: a
// transient database error must not take down NAV serving, which does not need
// this job to be correct.
type Snapshotter struct {
	store    Store
	interval time.Duration
	batch    int
	logger   *slog.Logger
	metrics  *SnapshotMetrics
	now      func() time.Time
}

// SnapshotMetrics is the checkpoint job's observability surface. Construct with
// NewSnapshotMetrics; a nil *SnapshotMetrics is inert, so tests need not
// register a collector.
type SnapshotMetrics struct {
	writes      *prometheus.CounterVec
	lastSuccess prometheus.Gauge
	stale       prometheus.Gauge
	// fullScans counts materializations that could NOT be served from a
	// checkpoint, by reason. It is incremented from the read path
	// (ObserveFullScan), not by this job, because that is where the cost lands.
	fullScans *prometheus.CounterVec
}

// NewSnapshotMetrics registers the checkpoint series on reg. A nil reg returns
// unregistered collectors, which still work — the metrics are inert, not the
// job.
func NewSnapshotMetrics(reg prometheus.Registerer) *SnapshotMetrics {
	m := &SnapshotMetrics{
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_accounting_snapshot_writes_total",
			Help: "ledger snapshot write attempts, by result (ok|error).",
		}, []string{"result"}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_accounting_snapshot_last_success_timestamp_seconds",
			Help: "unix time of the last successful snapshot write. Alert on its AGE: a job that stopped ticking leaves the error counter flat too.",
		}),
		stale: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_accounting_snapshot_stale_portfolios",
			Help: "portfolios past their snapshot watermark at the last pass. Pinned at the batch limit means the checkpoints are not catching up.",
		}),
		fullScans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_accounting_ledger_full_scans_total",
			Help: "book materializations that read the ENTIRE journal instead of a snapshot tail, by reason. Sustained non-zero is #229 reinstated.",
		}, []string{"reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.writes, m.lastSuccess, m.stale, m.fullScans)
	}
	return m
}

// ObserveFullScan records that a materialization fell back to the whole
// journal. Called from the read path; a no-op on a nil receiver or an empty
// reason (an empty reason means the read WAS bounded).
func (m *SnapshotMetrics) ObserveFullScan(reason FullScanReason) {
	if m == nil || reason == "" {
		return
	}
	m.fullScans.WithLabelValues(string(reason)).Inc()
}

// SnapshotterConfig configures the checkpoint job.
type SnapshotterConfig struct {
	// Interval between passes. Must be positive.
	Interval time.Duration
	// Batch is the most portfolios one pass will checkpoint. Must be positive:
	// an unbounded pass against a large estate would hold the pool for as long
	// as it took, which is the shape of problem this job exists to remove.
	Batch int
	// Metrics may be nil (the job runs uninstrumented).
	Metrics *SnapshotMetrics
	// Now is injectable for tests; nil ⇒ time.Now.
	Now func() time.Time
}

// NewSnapshotter validates the configuration and builds the job. A bad interval
// or batch is an error at the composition root, not a default that silently
// disables checkpointing — "nothing configured" and "checked, and fine" must
// not look the same, and a checkpoint job that quietly does nothing is the
// original defect wearing the fix's name.
func NewSnapshotter(store Store, logger *slog.Logger, cfg SnapshotterConfig) (*Snapshotter, error) {
	if store == nil {
		return nil, errors.New("ledger: snapshotter needs a store")
	}
	if logger == nil {
		return nil, errors.New("ledger: snapshotter needs a logger — its whole failure mode is silence")
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("ledger: snapshotter interval must be positive, got %s", cfg.Interval)
	}
	if cfg.Batch <= 0 {
		return nil, fmt.Errorf("ledger: snapshotter batch must be positive, got %d", cfg.Batch)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Snapshotter{
		store:    store,
		interval: cfg.Interval,
		batch:    cfg.Batch,
		logger:   logger,
		metrics:  cfg.Metrics,
		now:      now,
	}, nil
}

// Run checkpoints stale portfolios every interval until ctx is canceled. It
// returns ctx.Err() on cancellation and never on a pass failure: a database
// blip must not stop the job that keeps the read path bounded, and the metrics
// above are what escalate a persistent one.
func (s *Snapshotter) Run(ctx context.Context) error {
	s.logger.Info("ledger snapshotter starting", "interval", s.interval, "batch", s.batch)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if n, err := s.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error("ledger snapshot pass failed — NAV reads for the affected "+
					"portfolios are falling back to a FULL journal scan until it succeeds",
					"written", n, "err", err)
			}
		}
	}
}

// Once runs one checkpoint pass and returns how many snapshots it wrote. It
// keeps going after a per-portfolio failure — one bad portfolio must not stop
// the rest from being checkpointed — and returns the first error it hit.
func (s *Snapshotter) Once(ctx context.Context) (int, error) {
	stale, err := s.store.StalePortfolios(ctx, s.batch)
	if err != nil {
		return 0, fmt.Errorf("list stale portfolios: %w", err)
	}
	if s.metrics != nil {
		s.metrics.stale.Set(float64(len(stale)))
	}

	var (
		written  int
		firstErr error
	)
	for _, id := range stale {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if err := s.checkpoint(ctx, id); err != nil {
			s.logger.Error("ledger snapshot write failed", "portfolio_id", id, "err", err)
			if s.metrics != nil {
				s.metrics.writes.WithLabelValues("error").Inc()
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written++
		if s.metrics != nil {
			s.metrics.writes.WithLabelValues("ok").Inc()
			s.metrics.lastSuccess.Set(float64(s.now().Unix()))
		}
	}
	return written, firstErr
}

// checkpoint folds one portfolio's whole journal and stores the result.
//
// It reads the FULL journal on purpose — that is what building a checkpoint
// from an event-sourced log means, and it is the one place in the service
// entitled to. Deriving the watermarks from the entries actually folded, rather
// than from the clock, is what makes the checkpoint honest: `now()` would claim
// a knowledge watermark covering entries this pass never saw, and the tail read
// would then skip them forever.
func (s *Snapshotter) checkpoint(ctx context.Context, portfolioID string) error {
	events, err := s.store.Journal(ctx, portfolioID)
	if err != nil {
		return fmt.Errorf("journal %s: %w", portfolioID, err)
	}
	if len(events) == 0 {
		// Raced with nothing to fold. A snapshot of an empty journal has no
		// meaningful watermark and SaveSnapshot would refuse it.
		return nil
	}
	var through time.Time
	for _, e := range events {
		if e.Knowledge.After(through) {
			through = e.Knowledge
		}
	}
	book := Replay(portfolioID, events)
	// Book.Snapshot carries the MaxEffective fence off the book itself, so it
	// records what was folded rather than what the caller believed was folded.
	return s.store.SaveSnapshot(ctx, book.Snapshot(through))
}
