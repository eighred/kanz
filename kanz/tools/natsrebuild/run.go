package natsrebuild

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/replay"
)

// Outcome is what actually happened to one (tenant, topic) target.
//
// These three used to be one. A missing topic, an empty topic and a topic that
// replayed 10k events all ended the same way: a log line and exit 0. That is
// the DR failure this package is built to make impossible — an operator reading
// "nats-rebuild complete" could not tell a restored spine from a run that read
// topics which do not exist. "Nothing configured" and "checked, and fine" must
// never look the same.
type Outcome string

const (
	// OutcomeReplayed — the topic existed and published at least one event.
	OutcomeReplayed Outcome = "replayed"
	// OutcomeEmpty — the topic exists in Kafka but held nothing in the read
	// window. Legitimate (a quiet tenant, a short window), so not fatal, but it
	// is reported at WARN and counted: it is also what a wrong
	// NATS_REBUILD_SINCE looks like.
	OutcomeEmpty Outcome = "empty"
	// OutcomeMissing — the topic does not exist in Kafka. Nothing was ever
	// archived under this name: the tenant was never provisioned
	// (infra/kafka/tenancy.yaml), or the name is wrong. FATAL — this is the
	// state that used to drain nothing and exit 0.
	OutcomeMissing Outcome = "missing"
	// OutcomeFailed — the probe, the read or the publish errored. FATAL.
	OutcomeFailed Outcome = "failed"
)

// Fatal reports whether an outcome must fail the run.
func (o Outcome) Fatal() bool { return o == OutcomeMissing || o == OutcomeFailed }

// TopicChecker reports whether a Kafka topic exists.
//
// Existence is probed SEPARATELY from reading because the reader cannot tell
// the two apart: replay.Reader.start treats a topic with no partitions as an
// immediate EOF, which is byte-for-byte the same result as a topic that exists
// and is empty. A broker that cannot be reached must surface as an error, never
// as "missing" — a loud failure that names the wrong cause sends the operator
// to provision a topic that already exists.
type TopicChecker interface {
	Exists(ctx context.Context, topic string) (bool, error)
}

// Source is a bounded, closeable read of one topic. *replay.Reader satisfies it.
type Source interface {
	replay.Source
	Close() error
}

// OpenFunc opens a bounded read of one Kafka topic.
type OpenFunc func(ctx context.Context, topic string, rng replay.Range) (Source, error)

// Result is the per-target record that makes the outcome auditable.
type Result struct {
	Target
	Outcome   Outcome
	Published uint64
	Malformed uint64
	Err       error
}

// Report is the whole run: one Result per attempted target, in order.
type Report struct {
	Results []Result
	// Requested is the tenant set the run was asked for, so a tenant that
	// never produced a Result is still visible in the summary.
	Requested []string
}

// Published totals events republished across every target.
func (r Report) Published() uint64 {
	var n uint64
	for _, res := range r.Results {
		n += res.Published
	}
	return n
}

// TenantSummary rolls one tenant's results up. Attempted may be less than the
// tenant's target count when the run failed fast on an earlier target.
type TenantSummary struct {
	Tenant    string
	Attempted int
	Replayed  int
	Empty     int
	Published uint64
}

// Tenants returns one summary per REQUESTED tenant, in request order —
// including tenants with no results at all, which is what a fail-fast run
// leaves behind and exactly the state a per-topic log cannot show.
func (r Report) Tenants() []TenantSummary {
	idx := make(map[string]int, len(r.Requested))
	out := make([]TenantSummary, 0, len(r.Requested))
	for _, t := range r.Requested {
		idx[t] = len(out)
		out = append(out, TenantSummary{Tenant: t})
	}
	for _, res := range r.Results {
		i, ok := idx[res.Tenant]
		if !ok {
			idx[res.Tenant] = len(out)
			out = append(out, TenantSummary{Tenant: res.Tenant})
			i = len(out) - 1
		}
		out[i].Attempted++
		out[i].Published += res.Published
		switch res.Outcome {
		case OutcomeReplayed:
			out[i].Replayed++
		case OutcomeEmpty:
			out[i].Empty++
		}
	}
	return out
}

// BarrenTenants names every requested tenant that replayed no event on any
// topic. Not fatal on its own — a tenant can legitimately be quiet — but it is
// the shape of a misconfigured window or a tenant whose archiver never ran, so
// it is surfaced by name instead of hiding inside a zero total.
func (r Report) BarrenTenants() []string {
	var out []string
	for _, s := range r.Tenants() {
		if s.Published == 0 {
			out = append(out, s.Tenant)
		}
	}
	return out
}

// Runner drains every target onto the live spine.
//
// It FAILS FAST: the first missing topic, probe error, read error or publish
// error aborts the run and returns the partial Report with the error. That is
// the pre-existing single-tenant behaviour preserved deliberately — a rebuild
// is re-runnable (stream dedup + idempotent handlers), so stopping at the first
// broken target and being fixed is safer than pressing on and reporting a
// half-restored spine as done.
type Runner struct {
	Targets   []Target
	Checker   TopicChecker
	Open      OpenFunc
	Publisher bus.Publisher
	Since     time.Duration
	Now       time.Time
	Logger    *slog.Logger
	// Requested is the tenant set asked for; used to detect a requested
	// tenant with no targets at all. Defaults to the tenants in Targets.
	Requested []string
}

// Run executes every target in order. The returned Report is complete for the
// targets attempted even when the error is non-nil.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	requested := r.Requested
	if len(requested) == 0 {
		seen := map[string]bool{}
		for _, t := range r.Targets {
			if !seen[t.Tenant] {
				seen[t.Tenant] = true
				requested = append(requested, t.Tenant)
			}
		}
	}
	rep := Report{Requested: requested}

	if len(r.Targets) == 0 {
		return rep, errors.New("natsrebuild: no targets — the run would restore nothing and must not report success")
	}
	if err := RequireTenantCoverage(requested, r.Targets); err != nil {
		return rep, err
	}
	if r.Checker == nil {
		return rep, errors.New("natsrebuild: TopicChecker is nil — without an existence probe a missing topic is indistinguishable from an empty one")
	}
	if r.Open == nil {
		return rep, errors.New("natsrebuild: OpenFunc is nil")
	}
	if r.Publisher == nil {
		return rep, errors.New("natsrebuild: Publisher is nil")
	}
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}

	for _, target := range r.Targets {
		res := r.runTarget(ctx, target, now, log)
		rep.Results = append(rep.Results, res)

		// EVERY target gets a line naming its outcome, at a level that matches
		// it. The three states are the point: a reader scanning the Job's log
		// must be able to tell "restored", "existed but empty" and "not there
		// at all" apart without correlating counts.
		args := []any{
			"tenant", target.Tenant, "topic", target.Topic, "base", target.Base,
			"state", target.State, "outcome", string(res.Outcome),
			"published", res.Published, "malformed", res.Malformed,
		}
		switch res.Outcome {
		case OutcomeReplayed:
			log.Info("rebuilt topic", args...)
		case OutcomeEmpty:
			log.Warn("rebuilt topic: EXISTS BUT REPLAYED NOTHING in the read window "+
				"(legitimate for a quiet topic; also what a too-short NATS_REBUILD_SINCE looks like)", args...)
		default:
			log.Error("rebuild topic FAILED", append(args, "err", res.Err)...)
			return rep, res.Err
		}
	}
	return rep, nil
}

func (r *Runner) runTarget(ctx context.Context, target Target, now time.Time, log *slog.Logger) Result {
	res := Result{Target: target}

	exists, err := r.Checker.Exists(ctx, target.Topic)
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf(
			"natsrebuild: probe topic %q (tenant %q): %w", target.Topic, target.Tenant, err)
		return res
	}
	if !exists {
		res.Outcome, res.Err = OutcomeMissing, fmt.Errorf(
			"natsrebuild: topic %q does not exist in Kafka (tenant %q, archived name %q): nothing was "+
				"ever written under this name, so nothing can be restored from it. Either the tenant was "+
				"never provisioned (infra/kafka/tenancy.yaml provisions %s.* for a tenant) or the topic "+
				"list is wrong. Refusing to report a rebuild that restored nothing",
			target.Topic, target.Tenant, target.Base, target.Tenant)
		return res
	}

	src, err := r.Open(ctx, target.Topic, target.Window(r.Since, now))
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf(
			"natsrebuild: open topic %q (tenant %q): %w", target.Topic, target.Tenant, err)
		return res
	}
	p := &Pipeline{Source: src, Publisher: r.Publisher, Logger: log}
	stats, runErr := p.Run(ctx)
	_ = src.Close()

	res.Published, res.Malformed = stats.Published, stats.Malformed
	if runErr != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf(
			"natsrebuild: rebuild topic %q (tenant %q, published %d before failing): %w",
			target.Topic, target.Tenant, stats.Published, runErr)
		return res
	}
	if stats.Published == 0 {
		res.Outcome = OutcomeEmpty
		return res
	}
	res.Outcome = OutcomeReplayed
	return res
}

// String renders a TenantSummary for the run's final log line.
func (s TenantSummary) String() string {
	return fmt.Sprintf("%s{topics=%d replayed=%d empty=%d published=%d}",
		s.Tenant, s.Attempted, s.Replayed, s.Empty, s.Published)
}

// Summary is the one-line, per-tenant account of the run.
func (r Report) Summary() string {
	parts := make([]string, 0, len(r.Requested))
	for _, s := range r.Tenants() {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, " ")
}
