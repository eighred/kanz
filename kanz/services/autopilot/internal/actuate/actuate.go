// Package actuate is the autopilot's infrastructure actuation (AUTO-01c): it
// closes the loop INFRA-01c (KEDA autoscaling) and DR-01d (regional failover)
// opened. Those react to lag and outages by adding workers or failing over;
// autopilot decides WHEN to pull those levers from the signal stream, e.g. scale
// inference on a circuit-open, scale ingest on staleness, fail over on a
// sustained SLO burn. Decoupled from any concrete client behind Scaler /
// Failover seams — the default impls log + record, a deployment wires the k8s
// client (patch a Deployment/ScaledObject) or the DR-01d failover trigger.
package actuate

import (
	"context"
	"log/slog"
	"sync"

	"github.com/kanz-eng/kanz/services/autopilot/internal/runbook"
	"github.com/kanz-eng/kanz/services/autopilot/internal/signal"
)

// Scaler nudges a target's capacity out in response to one signal.
//
// A ScaleOut implementation MUST be idempotent per (target, signal) — see
// runbook.Action's contract, which every ScaleOutAction step inherits.
// "Idempotent" here can ONLY mean converging the target toward a desired
// capacity derived from the target's OBSERVED CURRENT STATE — a
// read-then-set / desired-replica-count patch (e.g. "read the ScaledObject's
// current replica count, patch it to max(current, next-step-up)"). It must
// NOT apply a blind relative increment ("+1 replica").
//
// # Why a relative nudge is not just imperfect but wrong
//
// Signal delivery is at-least-once (see runbook.Action), so the same
// scale-out for the same incident WILL arrive more than once — on
// redelivery, on a retried step, or because two overlapping signals both
// map to the same runbook step. A read-then-set implementation converges to
// the same desired capacity no matter how many times it is called. A
// relative +1 implementation does not: each redelivery compounds on the
// last, scaling a service out repeatedly off what is, from the operator's
// point of view, one incident — the exact failure mode idempotency exists
// to prevent.
//
// The INFRA-01d ResourceQuota / KEDA maxReplicaCount clamp bounds the blast
// radius of that compounding (it cannot scale past the ceiling), but a
// clamped wrong answer is still wrong — the clamp is a backstop, not a
// substitute for computing the right target.
type Scaler interface {
	ScaleOut(ctx context.Context, target, reason string) error
}

// Failover triggers the DR-01d regional failover for a target.
//
// Unlike Scaler, an absolute-target Failover implementation is naturally
// idempotent without needing a read-then-set patch: "target" already names
// the desired end state (e.g. "make region X active"), not a relative
// direction, so repeating the call on redelivery converges to the same
// state instead of compounding. An implementation still must not treat a
// second call as "fail over again from wherever we ended up" — it must
// re-assert "be in region X," which is a no-op if already there.
type Failover interface {
	Failover(ctx context.Context, target, reason string) error
}

// ScaleOutAction wraps a Scaler as a runbook step.
func ScaleOutAction(s Scaler, target string) runbook.Action {
	return runbook.Action{
		Name: "scale_out_" + target,
		Run: func(ctx context.Context, sig signal.Signal) error {
			return s.ScaleOut(ctx, target, string(sig.Kind)+": "+sig.Summary)
		},
	}
}

// FailoverAction wraps a Failover as a runbook step. The region is the
// `region` attribute, falling back to the signal subject.
func FailoverAction(f Failover) runbook.Action {
	return runbook.Action{
		Name: "failover",
		Run: func(ctx context.Context, sig signal.Signal) error {
			target := sig.Attr("region")
			if target == "" {
				target = sig.Subject
			}
			return f.Failover(ctx, target, string(sig.Kind)+": "+sig.Summary)
		},
	}
}

// LogScaler is the default Scaler: records + logs scale-out intents. It is a
// logging stub for dev/test wiring (cmd/autopilot's only Scaler today, and
// the fixture controller_test.go's ScaleOuts assertions read), NOT a
// reference implementation of the Scaler contract above — counts increments
// per call by design, because tests need to prove a step ran. A production
// Scaler must still satisfy Scaler's idempotency contract; this type is
// exempt because it never actuates anything real.
type LogScaler struct {
	logger *slog.Logger
	mu     sync.Mutex
	counts map[string]int
}

func NewLogScaler(logger *slog.Logger) *LogScaler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogScaler{logger: logger, counts: map[string]int{}}
}

func (s *LogScaler) ScaleOut(ctx context.Context, target, reason string) error {
	s.mu.Lock()
	s.counts[target]++
	s.mu.Unlock()
	s.logger.WarnContext(ctx, "autopilot scaled out", "target", target, "reason", reason)
	return nil
}

func (s *LogScaler) ScaleOuts(target string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[target]
}

// LogFailover is the default Failover: records + logs failover intents.
type LogFailover struct {
	logger *slog.Logger
	mu     sync.Mutex
	done   map[string]string
}

func NewLogFailover(logger *slog.Logger) *LogFailover {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogFailover{logger: logger, done: map[string]string{}}
}

func (f *LogFailover) Failover(ctx context.Context, target, reason string) error {
	f.mu.Lock()
	f.done[target] = reason
	f.mu.Unlock()
	f.logger.WarnContext(ctx, "autopilot triggered failover", "target", target, "reason", reason)
	return nil
}

func (f *LogFailover) FailedOver(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.done[target]
	return ok
}
