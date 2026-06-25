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

// Scaler nudges a target's capacity. Direction is +1 (scale out) here; the
// concrete impl clamps to the INFRA-01d ResourceQuota / KEDA maxReplicaCount so
// autopilot can never breach the configured ceiling.
type Scaler interface {
	ScaleOut(ctx context.Context, target, reason string) error
}

// Failover triggers the DR-01d regional failover for a target.
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

// LogScaler is the default Scaler: records + logs scale-out intents.
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
