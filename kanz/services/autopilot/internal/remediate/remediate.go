// Package remediate is the autopilot's data-plane remediation (AUTO-01b): data
// quarantine on a DATA-05 reconciliation divergence (or a critical gap), and
// model auto-rollback on drift — MLOPS-01e promotion run in reverse. The actions
// are decoupled from any concrete client behind Quarantiner / ModelRoller seams
// (the repo's "inject the side-effect, log by default" stance, as DEBT-02): the
// default impls record + log, and a deployment wires the real command publisher
// (a quarantine COMMAND on the subject's domain, a model-rollback COMMAND on
// platform.model).
package remediate

import (
	"context"
	"log/slog"
	"sync"

	"github.com/eighred/kanz/services/autopilot/internal/runbook"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Quarantiner halts processing of a data subject so divergent/bad data stops
// propagating into derived state until a human clears it.
type Quarantiner interface {
	Quarantine(ctx context.Context, subject, reason string) error
	// Quarantined reports whether subject is currently quarantined (idempotency +
	// inspection).
	Quarantined(subject string) bool
}

// ModelRoller reverts a model to its last-known-good version — the MLOPS-01e
// promotion gate run backwards.
type ModelRoller interface {
	Rollback(ctx context.Context, modelID, reason string) error
}

// QuarantineAction wraps a Quarantiner as a runbook step. Idempotent: a
// re-quarantine of an already-quarantined subject is a no-op in the default impl.
func QuarantineAction(q Quarantiner) runbook.Action {
	return runbook.Action{
		Name: "quarantine_subject",
		Run: func(ctx context.Context, s signal.Signal) error {
			return q.Quarantine(ctx, s.Subject, string(s.Kind)+": "+s.Summary)
		},
	}
}

// RollbackAction wraps a ModelRoller as a runbook step. The model id is the
// signal subject (drift's subject is the model), or the `model_id` attribute.
func RollbackAction(r ModelRoller) runbook.Action {
	return runbook.Action{
		Name: "model_rollback",
		Run: func(ctx context.Context, s signal.Signal) error {
			modelID := s.Attr("model_id")
			if modelID == "" {
				modelID = s.Subject
			}
			return r.Rollback(ctx, modelID, string(s.Kind)+": "+s.Summary)
		},
	}
}

// LogQuarantiner is the default Quarantiner: it records the quarantine set and
// logs loudly. A real deployment swaps in a command-publishing impl.
type LogQuarantiner struct {
	logger *slog.Logger
	mu     sync.Mutex
	set    map[string]string // subject -> reason
}

func NewLogQuarantiner(logger *slog.Logger) *LogQuarantiner {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogQuarantiner{logger: logger, set: map[string]string{}}
}

func (q *LogQuarantiner) Quarantine(ctx context.Context, subject, reason string) error {
	q.mu.Lock()
	_, already := q.set[subject]
	q.set[subject] = reason
	q.mu.Unlock()
	if !already {
		q.logger.WarnContext(ctx, "autopilot quarantined data subject", "subject", subject, "reason", reason)
	}
	return nil
}

func (q *LogQuarantiner) Quarantined(subject string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.set[subject]
	return ok
}

// LogModelRoller is the default ModelRoller: records + logs the rollback.
type LogModelRoller struct {
	logger *slog.Logger
	mu     sync.Mutex
	rolled map[string]string // modelID -> reason
}

func NewLogModelRoller(logger *slog.Logger) *LogModelRoller {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogModelRoller{logger: logger, rolled: map[string]string{}}
}

func (r *LogModelRoller) Rollback(ctx context.Context, modelID, reason string) error {
	r.mu.Lock()
	r.rolled[modelID] = reason
	r.mu.Unlock()
	r.logger.WarnContext(ctx, "autopilot rolled back model", "model_id", modelID, "reason", reason)
	return nil
}

func (r *LogModelRoller) RolledBack(modelID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rolled[modelID]
	return ok
}
