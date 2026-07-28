// Package runbook is the autopilot's runbook-as-code (AUTO-01d): each recognized
// operational condition maps to an ordered sequence of remediation actions. The
// Matcher recognizes conditions from signals (with a severity threshold, so only
// actionable conditions auto-remediate); the Registry binds each to its runbook.
// A signal that matches no runbook is a novel/unrecognized condition the
// controller escalates to a human rather than acting on blindly.
package runbook

import (
	"context"

	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Action is one remediation step. It MUST be idempotent — a runbook can be
// re-run on redelivery (at-least-once signals) and a step can be retried.
type Action struct {
	Name string
	Run  func(ctx context.Context, s signal.Signal) error
}

// Runbook is the codified response to one condition: its steps run in order,
// stopping at the first failure (which the controller escalates).
type Runbook struct {
	Condition string
	Steps     []Action
}

// Rule recognizes a condition from a signal kind at or above a severity floor.
// The floor is what makes remediation conservative: a WARNING gap is below the
// auto-quarantine threshold, so it falls through to escalation (a human decides)
// rather than triggering an actuator on a soft signal.
type Rule struct {
	Kind        signal.Kind
	MinSeverity signal.Severity
	Condition   string
}

// Matcher resolves a signal to a recognized condition.
type Matcher struct {
	rules []Rule
}

func NewMatcher(rules ...Rule) *Matcher { return &Matcher{rules: rules} }

// Match returns the condition for s, or ok=false when no rule recognizes it at a
// sufficient severity — a novel/unrecognized condition.
func (m *Matcher) Match(s signal.Signal) (string, bool) {
	for _, r := range m.rules {
		if r.Kind == s.Kind && s.Severity >= r.MinSeverity {
			return r.Condition, true
		}
	}
	return "", false
}

// Registry maps a condition to its runbook.
type Registry struct {
	books map[string]*Runbook
}

func NewRegistry() *Registry { return &Registry{books: map[string]*Runbook{}} }

// Register binds a runbook to its condition.
func (r *Registry) Register(rb *Runbook) { r.books[rb.Condition] = rb }

// Lookup returns the runbook for a condition.
func (r *Registry) Lookup(condition string) (*Runbook, bool) {
	rb, ok := r.books[condition]
	return rb, ok
}
