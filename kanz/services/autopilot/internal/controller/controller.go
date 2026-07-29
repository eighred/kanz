// Package controller is the autopilot control loop (AUTO-01a): it consumes the
// signal stream, matches each signal to a recognized condition, runs that
// condition's runbook, and escalates to a human on anything it does not
// recognize or cannot remediate (AUTO-01d). It is the event-driven brain the
// remediate/actuate actions are the hands of.
package controller

import (
	"context"
	"fmt"
	"log/slog"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/services/autopilot/internal/runbook"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Escalator hands a condition to a human (page/ticket) — the AUTO-01d
// human-in-the-loop. Reached for a novel/unrecognized condition or a remediation
// that failed, so a person decides rather than the system acting blindly.
type Escalator interface {
	Escalate(ctx context.Context, s signal.Signal, reason string) error
}

// Outcome is what the loop did with a signal — for tests + metrics.
type Outcome string

const (
	OutcomeRemediated Outcome = "remediated"
	OutcomeEscalated  Outcome = "escalated"
)

// Controller folds signals into remediation or escalation.
type Controller struct {
	matcher   *runbook.Matcher
	registry  *runbook.Registry
	escalator Escalator
	logger    *slog.Logger
	metrics   *Metrics
}

func New(m *runbook.Matcher, r *runbook.Registry, e Escalator, logger *slog.Logger, metrics *Metrics) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{matcher: m, registry: r, escalator: e, logger: logger, metrics: metrics}
}

// Handle matches bus.EventHandler. It classifies the event and, when it is an
// operational signal, dispatches it. Non-signal events (the vast majority) are
// ignored — acked without action. A dispatch that ends in escalation is still a
// handled outcome (ack); only a failure to ESCALATE (the human couldn't be
// reached) returns an error so the bus retries.
func (c *Controller) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	s, ok := signal.Classify(env, payload)
	if !ok {
		return nil
	}
	_, err := c.Dispatch(ctx, s)
	return err
}

// Dispatch runs the control loop for one signal, returning what it did. An error
// is returned only when escalation itself fails (retryable); a remediation-step
// failure is converted into an escalation, not an error.
func (c *Controller) Dispatch(ctx context.Context, s signal.Signal) (Outcome, error) {
	cond, ok := c.matcher.Match(s)
	if !ok {
		return c.escalate(ctx, s, fmt.Sprintf("unrecognized condition: kind=%s severity=%s", s.Kind, s.Severity))
	}
	rb, ok := c.registry.Lookup(cond)
	if !ok {
		return c.escalate(ctx, s, "no runbook registered for condition "+cond)
	}

	for _, step := range rb.Steps {
		if err := step.Run(ctx, s); err != nil {
			c.logger.ErrorContext(ctx, "autopilot remediation step failed",
				"condition", cond, "step", step.Name, "subject", s.Subject, "err", err)
			return c.escalate(ctx, s, fmt.Sprintf("remediation step %q failed: %v", step.Name, err))
		}
	}

	c.metrics.incOutcome(cond, OutcomeRemediated)
	c.logger.InfoContext(ctx, "autopilot remediated condition",
		"condition", cond, "subject", s.Subject, "severity", s.Severity.String(),
		"steps", len(rb.Steps), "event_id", s.EventID)
	return OutcomeRemediated, nil
}

func (c *Controller) escalate(ctx context.Context, s signal.Signal, reason string) (Outcome, error) {
	cond := string(s.Kind)
	c.metrics.incOutcome(cond, OutcomeEscalated)
	c.logger.WarnContext(ctx, "autopilot escalating to human",
		"kind", s.Kind, "subject", s.Subject, "severity", s.Severity.String(), "reason", reason)
	if err := c.escalator.Escalate(ctx, s, reason); err != nil {
		return OutcomeEscalated, fmt.Errorf("escalate %s: %w", s.Kind, err)
	}
	return OutcomeEscalated, nil
}
