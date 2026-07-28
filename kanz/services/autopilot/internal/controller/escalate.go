package controller

import (
	"context"
	"log/slog"
	"sync"

	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Escalation is one recorded hand-off to a human.
type Escalation struct {
	Signal signal.Signal
	Reason string
}

// LogEscalator is the default Escalator: it logs at error level (so the platform
// alerting routes it to an on-call human) and records the escalation for
// inspection. A deployment swaps in a pager/ticket impl behind the Escalator
// seam; logging is a real sink (OBS-01a ships stdout JSON) and the safe default.
type LogEscalator struct {
	logger *slog.Logger
	mu     sync.Mutex
	seen   []Escalation
}

func NewLogEscalator(logger *slog.Logger) *LogEscalator {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogEscalator{logger: logger}
}

func (e *LogEscalator) Escalate(ctx context.Context, s signal.Signal, reason string) error {
	e.mu.Lock()
	e.seen = append(e.seen, Escalation{Signal: s, Reason: reason})
	e.mu.Unlock()
	e.logger.LogAttrs(ctx, slog.LevelError, "autopilot human escalation",
		slog.String("kind", string(s.Kind)),
		slog.String("subject", s.Subject),
		slog.String("severity", s.Severity.String()),
		slog.String("reason", reason),
		slog.String("event_id", s.EventID))
	return nil
}

// Escalations returns the recorded escalations (test/inspection).
func (e *LogEscalator) Escalations() []Escalation {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Escalation, len(e.seen))
	copy(out, e.seen)
	return out
}
