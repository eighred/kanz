package controller

import (
	"context"
	"log/slog"

	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// LogEscalator is the default Escalator: it logs at error level, so the
// platform's alerting routes the hand-off to an on-call human. A deployment
// swaps in a pager/ticket impl behind the Escalator seam; logging is a real sink
// (OBS-01a ships stdout JSON) and the safe default.
//
// IT RETAINS NOTHING (#844). It used to append every escalation to a
// `seen []Escalation` under a mutex — no cap, no TTL, no eviction — read only by
// a helper documented "(test/inspection)" that no production code called. Each
// entry pinned a whole signal.Signal plus the reason string for the life of the
// pod, and the accumulation rate peaked during a degraded period (a flapping
// venue, a stalled consumer, an incident), which is exactly when an OOM kill of
// the remediation controller costs the most and explains the least: the
// component whose job is noticing failures restarts, with no error anyone can
// trace to a cause.
//
// It was deleted rather than capped because escalations already have two
// production sinks and this was not one of them: the error-level line below, and
// the kanz_autopilot_outcomes_total{outcome="escalated"} counter in metrics.go.
// Nothing could ever have read the buffer — autopilot serves /healthz, /readyz
// and /metrics and nothing else (internal/server/server.go), so there was no
// surface an operator could reach it through. A bounded ring would have kept the
// heap cost and the third answer while still having no reader.
//
// If in-process retention is ever genuinely wanted, the shape to copy is
// internal/risk/state's dedupWindow — an explicit cap, a TTL and a gc() — so the
// bound exists in production rather than only in the assertion that reads it.
// The mutex went with the slice: a lock over immutable state advertises a
// protection that is not doing anything. *slog.Logger is safe for concurrent
// use, so Escalate needs none.
type LogEscalator struct {
	logger *slog.Logger
}

func NewLogEscalator(logger *slog.Logger) *LogEscalator {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogEscalator{logger: logger}
}

func (e *LogEscalator) Escalate(ctx context.Context, s signal.Signal, reason string) error {
	e.logger.LogAttrs(ctx, slog.LevelError, "autopilot human escalation",
		slog.String("kind", string(s.Kind)),
		slog.String("subject", s.Subject),
		slog.String("severity", s.Severity.String()),
		slog.String("reason", reason),
		slog.String("event_id", s.EventID))
	return nil
}
