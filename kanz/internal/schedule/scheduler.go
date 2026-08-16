// Package schedule drives model-calibration refreshes on a fixed operational
// cadence (WIRE-01c). The PARITY-03a calibrators (curve/vol) pull quotes from a
// point-in-time QuoteSource, fit, and publish the result to a point-in-time
// Store — but nothing drove Refresh on a schedule, so the live snapshot never
// reached the calibrators in a running process. This is that driver.
//
// The scheduler owns no calibration logic; it is a generic cadence loop over a
// set of named Refresh jobs. deny-on-garbage is the calibrator's contract, not
// the scheduler's: a Refresh error (stale/empty/arbitrageable quotes) leaves the
// prior point-in-time version serving, so the scheduler only has to log the
// failure and keep ticking — a bad tick never unpublishes a good curve.
// PROMOTED OUT OF internal/risk/pricing (2026-08-16). It lived there because the
// curve calibrator was its only caller, and CLAUDE.md's rule is that shared code
// starts in one service's internal/ and moves only when a SECOND consumer
// appears. That consumer is the bar rollup in market-data.
//
// The move was not optional once market-data needed it:
// test/arch/risk_boundary_test.go forbids code outside kanz/internal/risk/ from
// importing anything under it except api/v*, with risk-engine exempted as the
// composer. So the alternatives were a second ticker implementation in
// market-data — the copied helper this estate has paid for before — or this.
// Nothing about the scheduler was ever risk-specific.

package schedule

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// RefreshFunc calibrates one target as of asOf and publishes the result to its
// store, returning an error the scheduler logs and swallows (the prior version
// keeps serving). It is exactly the shape of curve.Calibrator.Refresh /
// volsurface.Calibrator.Refresh once the target id is closed over.
type RefreshFunc func(ctx context.Context, asOf time.Time) error

// Job is one calibration target on one cadence: e.g. the USD rate curve on the
// 5-minute intraday interval, or the same curve on the nightly-close interval.
// The same target appears as two Jobs when it refreshes on two cadences.
type Job struct {
	// Name identifies the job in logs (e.g. "curve/USD/intraday").
	Name string
	// Interval is the refresh cadence. A non-positive interval disables the job
	// (it is skipped at construction) — the composition root's "off unless
	// configured" knob.
	Interval time.Duration
	// Refresh performs the calibration. Required.
	Refresh RefreshFunc
}

// Scheduler ticks each Job's Refresh on its own interval until the context is
// canceled. It is safe to construct with an empty job set (Run returns
// immediately) so the composition root can wire it unconditionally.
type Scheduler struct {
	jobs   []Job
	now    func() time.Time
	logger *slog.Logger
}

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithClock injects the as-of clock (test seam); the default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger sets the logger; the default is slog.Default.
func WithLogger(l *slog.Logger) Option {
	return func(s *Scheduler) {
		if l != nil {
			s.logger = l
		}
	}
}

// New returns a Scheduler over jobs with a positive interval and a non-nil
// Refresh; jobs failing either check are dropped (a disabled cadence, or a
// misconfiguration the composition root should not have produced).
func New(jobs []Job, opts ...Option) *Scheduler {
	s := &Scheduler{now: time.Now, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	for _, j := range jobs {
		if j.Interval > 0 && j.Refresh != nil {
			s.jobs = append(s.jobs, j)
		}
	}
	return s
}

// Jobs reports how many enabled jobs the scheduler will run — the composition
// root logs it so an all-disabled configuration is visible at startup.
func (s *Scheduler) Jobs() int { return len(s.jobs) }

// Run drives every enabled job until ctx is canceled, then waits for the
// in-flight refreshes to unwind and returns nil. Each job runs once immediately
// (so a restart warms the stores without waiting a full interval) and then on
// its ticker. Refreshes for one job are serialized (never overlapping); distinct
// jobs run concurrently. Run blocks; callers put it on its own goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, j := range s.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			s.runJob(ctx, j)
		}(j)
	}
	wg.Wait()
	return nil
}

// runJob refreshes j immediately, then on every interval tick, until ctx is
// done. The immediate run is skipped-with-log semantics only for errors: the
// calibrator itself preserves the prior version on failure.
func (s *Scheduler) runJob(ctx context.Context, j Job) {
	s.fire(ctx, j)
	ticker := time.NewTicker(j.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fire(ctx, j)
		}
	}
}

// fire runs one refresh, logging (but not propagating) an error. A canceled
// context mid-refresh is not an operational failure, so it logs at debug.
func (s *Scheduler) fire(ctx context.Context, j Job) {
	asOf := s.now()
	if err := j.Refresh(ctx, asOf); err != nil {
		if ctx.Err() != nil {
			s.logger.Debug("calibration refresh canceled", "job", j.Name)
			return
		}
		// deny-on-garbage: the prior point-in-time version keeps serving.
		s.logger.Warn("calibration refresh failed; prior version still serving",
			"job", j.Name, "as_of", asOf, "err", err)
		return
	}
	s.logger.Debug("calibration refresh ok", "job", j.Name, "as_of", asOf)
}
