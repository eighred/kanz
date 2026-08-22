// Package verify runs the AUDIT-01b hash-chain tamper check on a schedule
// (#665).
//
// # What was missing
//
// The chain had a tamper check and nothing ran it. infra/deploy/audit-deploy.yaml
// granted kanz-monitoring an authority on the strength of a schedule that did not
// exist, and the manifest's own words name the state that left behind: "tamper
// detection depending on somebody remembering to look".
//
// # Why this runs in-process rather than as a job calling the HTTP endpoint
//
// The issue proposed a runner calling GET /v1/audit/verify through the gateway.
// Two things argue against it, and the second is the substantive one.
//
// FIRST, THE CREDENTIAL DOES NOT EXIST. identity issues tokens through POST
// /login — a subject and an Argon2 credential against a user store. There is no
// client-credentials flow and no machine principal, so a scheduled caller would
// need a pseudo-user holding a long-lived password, in a secret, carrying an
// estate-wide read authority. That is a new credential pattern and a security
// decision, not a wiring change.
//
// SECOND, THE GATEWAY HOP ADDS A TRIGGER AND NOT TRUST. /v1/audit/verify is
// computed BY this service either way — the handler calls the same report.Verify
// this scheduler does. An external caller changes WHO SCHEDULES the check, not
// who attests to it, so it buys no independence over the thing being attested.
// What it would buy is a second failure mode: a CronJob that silently stops.
//
// The failure an external runner IS better at — this process dying and taking
// the check with it — is covered instead by publishing WHEN the last successful
// verification happened, and alerting on its age. That alert fires for a dead
// verifier, a wedged one, and a pod that never started one, which is a superset
// of what "the CronJob did not run" would have caught.
package verify

import (
	"context"
	"log/slog"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/eighred/kanz/services/audit/internal/report"
)

// Outcome is what one verification concluded.
//
// THERE ARE THREE OF THESE AND COLLAPSING ANY PAIR IS THE DEFECT. An unreadable
// store reported as intact is a tamper check that passes while the database is
// down; reported as broken it pages somebody to investigate a forgery that has
// not happened. "I could not tell" is a real answer and it has its own name.
type Outcome string

const (
	// OutcomeIntact — the chain verified over every record.
	OutcomeIntact Outcome = "intact"
	// OutcomeBroken — the chain did NOT verify. A verdict, not a failure.
	OutcomeBroken Outcome = "broken"
	// OutcomeError — the store could not be read, so nothing was verified.
	OutcomeError Outcome = "error"
)

// DefaultInterval is how often the chain is verified when nothing says
// otherwise.
//
// ONE HOUR, AND THE NUMBER IS BOUNDED BY THE COST OF THE CHECK RATHER THAN BY
// TASTE. report.Verify is a WHOLE-LOG SCAN holding one pool connection for its
// duration — its own doc says so, and says why it cannot be a suffix: "a hash
// chain cannot be verified from a suffix without a trusted anchor for everything
// before it". So the interval is the frequency at which a full scan is
// affordable, not the frequency at which detection would be nicest.
//
// THERE IS NO WAY TO TURN IT OFF, deliberately. An operator who lengthens the
// interval past the staleness threshold makes the staleness alert fire, which is
// the correct outcome: a chain nobody is verifying should be visible as such
// rather than configurable into silence.
const DefaultInterval = time.Hour

// Result is one verification, in the shape the caller reports on.
type Result struct {
	Outcome Outcome
	// Records is how many records the attestation covered. Zero on an error,
	// where nothing was read.
	Records int
	// Detail locates a break — "chain broken at index N" — and is empty
	// otherwise. It is what an alert has to say WHERE.
	Detail string
	// Err is the store error for OutcomeError, nil otherwise. A broken chain
	// carries no Err: it is a successful verification with a bad answer.
	Err error
	// At is when the verification completed.
	At time.Time
}

// Scheduler verifies the hash chain on an interval.
type Scheduler struct {
	store    audit.Store
	interval time.Duration
	now      func() time.Time
	logger   *slog.Logger
	observer func(Result)
}

// Option customizes a Scheduler.
type Option func(*Scheduler)

// WithInterval sets the verification interval; <=0 keeps DefaultInterval.
func WithInterval(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithClock replaces the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger sets the logger; nil keeps slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Scheduler) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithObserver is called with every Result — the seam the composition root turns
// into metrics.
//
// Nil ⇒ not observed, and then the check is LOG-ONLY, which the issue rules out
// as detection: "a verifier that logs a failure into the same estate nobody is
// watching is not detection". The composition root wires it; this package does
// not import prometheus so it stays testable without a registry.
func WithObserver(fn func(Result)) Option {
	return func(s *Scheduler) { s.observer = fn }
}

// New builds a Scheduler over store.
func New(store audit.Store, opts ...Option) *Scheduler {
	s := &Scheduler{
		store:    store,
		interval: DefaultInterval,
		now:      time.Now,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Once runs a single verification and reports it to the observer.
func (s *Scheduler) Once(ctx context.Context) Result {
	att, err := report.Verify(ctx, s.store)
	res := Result{At: s.now()}
	switch {
	case err != nil:
		// NOTHING WAS VERIFIED. Not intact, not broken — unread.
		res.Outcome, res.Err = OutcomeError, err
		s.logger.ErrorContext(ctx, "audit: the hash chain could not be read, so it was NOT verified "+
			"— this is neither a pass nor a tamper, and the chain's integrity is currently unknown",
			"err", err)
	case !att.Verified:
		res.Outcome, res.Records, res.Detail = OutcomeBroken, att.Records, att.Detail
		// The loudest line this service can write. A broken chain means a record
		// in the compliance log does not hash to its successor, which is either a
		// bug in the writer or somebody editing the log.
		s.logger.ErrorContext(ctx, "audit: THE HASH CHAIN DID NOT VERIFY — the compliance log has "+
			"been altered or a record was written incorrectly. Every attestation derived from it is "+
			"in question until this is explained",
			"detail", att.Detail, "records", att.Records)
	default:
		res.Outcome, res.Records = OutcomeIntact, att.Records
		s.logger.InfoContext(ctx, "audit: hash chain verified",
			"records", att.Records, "interval", s.interval.String())
	}
	if s.observer != nil {
		s.observer(res)
	}
	return res
}

// Run verifies on the interval until ctx is done, returning ctx.Err().
//
// IT VERIFIES IMMEDIATELY, BEFORE THE FIRST TICK. A ticker-only loop is the
// classic shape of this bug: a pod restarting more often than the interval never
// verifies at all, so the estate looks scheduled while nothing is ever checked —
// which is the failure this whole package exists to end, and would be a poor
// thing to reintroduce inside the fix.
func (s *Scheduler) Run(ctx context.Context) error {
	s.Once(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.Once(ctx)
		}
	}
}
