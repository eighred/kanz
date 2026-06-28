package posttrade

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Severity grades a settlement fail by how long it has aged unsettled — the
// escalation tier the AUTO-01 controller keys remediation on (mirrors
// settlement.v1.FailSeverity).
type Severity int

const (
	SeverityNone Severity = iota
	SeverityWarning
	SeverityCritical
	SeverityEscalated
)

func (s Severity) String() string {
	switch s {
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	case SeverityEscalated:
		return "escalated"
	default:
		return "none"
	}
}

// AgingPolicy maps the number of days a settlement has aged past its settlement
// date to an escalation severity. The defaults: within the grace window ⇒
// WARNING, beyond it ⇒ CRITICAL, far beyond ⇒ ESCALATED.
type AgingPolicy struct {
	// GraceDays is the window past the settlement date before a pending
	// instruction is treated as a fail at all (T+N markets allow a short lag).
	GraceDays int
	// CriticalDays / EscalatedDays are the age thresholds (inclusive) for the
	// CRITICAL and ESCALATED tiers.
	CriticalDays  int
	EscalatedDays int
}

// DefaultAgingPolicy is a conservative default: a 1-day grace, CRITICAL at 3 days
// aged, ESCALATED at 6.
var DefaultAgingPolicy = AgingPolicy{GraceDays: 1, CriticalDays: 3, EscalatedDays: 6}

// severity returns the tier for an age in days past the settlement date, or
// SeverityNone when within the grace window.
func (p AgingPolicy) severity(ageDays int) Severity {
	switch {
	case ageDays >= p.EscalatedDays:
		return SeverityEscalated
	case ageDays >= p.CriticalDays:
		return SeverityCritical
	case ageDays > p.GraceDays:
		return SeverityWarning
	default:
		return SeverityNone
	}
}

// Fail is the working shape behind settlement.v1.SettlementFail — a settlement
// that failed outright or aged past its settlement date unsettled. It is the
// operational signal POST-01d emits: AUTO-01 consumes it (severity → remediation)
// and IBOR-01e consumes it (an expected position/cash movement that did not occur).
type Fail struct {
	InstructionID  string
	InstrumentID   string
	Counterparty   string
	SettlementDate time.Time
	AgeDays        int
	Severity       Severity
	Reason         string
	DetectedAt     time.Time
}

// DetectFails scans settlements and returns the ones that have failed or aged
// past their settlement date beyond the policy grace window, as of now. An
// already-FAILED settlement is always reported (with its age); an INSTRUCTED one
// is reported once it ages beyond the grace window. SETTLED and pre-instruction
// settlements are never fails. The result is ordered by descending severity then
// instruction id, so the worst fails surface first.
func DetectFails(settlements []*Settlement, policy AgingPolicy, now time.Time) []Fail {
	var fails []Fail
	for _, s := range settlements {
		age := daysBetween(s.SettlementDate, now)
		switch s.Status {
		case StatusFailed:
			sev := policy.severity(age)
			if sev == SeverityNone {
				sev = SeverityWarning // an explicit fail is at least a warning regardless of age
			}
			reason := s.FailReason
			if reason == "" {
				reason = "settlement failed"
			}
			fails = append(fails, newFail(s, age, sev, reason, now))
		case StatusInstructed:
			if sev := policy.severity(age); sev != SeverityNone {
				fails = append(fails, newFail(s, age, sev,
					fmt.Sprintf("instruction unsettled %d day(s) past settlement date", age), now))
			}
		}
	}
	sort.Slice(fails, func(i, j int) bool {
		if fails[i].Severity != fails[j].Severity {
			return fails[i].Severity > fails[j].Severity
		}
		return fails[i].InstructionID < fails[j].InstructionID
	})
	return fails
}

func newFail(s *Settlement, age int, sev Severity, reason string, now time.Time) Fail {
	return Fail{
		InstructionID:  s.InstructionID,
		InstrumentID:   s.InstrumentID,
		Counterparty:   s.Counterparty,
		SettlementDate: s.SettlementDate,
		AgeDays:        age,
		Severity:       sev,
		Reason:         reason,
		DetectedAt:     now,
	}
}

// FailSink is the publish seam for settlement-fail FACTs (POST-01d). The concrete
// bus emitter that maps a Fail onto the settlement.v1.SettlementFail FACT the
// AUTO-01 controller and the IBOR-01e reconciliation consume is wired at the
// composition root (the DEBT-02 inject-the-side-effect stance); the LogSink here
// is the dependency-free default.
type FailSink interface {
	Publish(ctx context.Context, f Fail) error
}

// FailSinkFunc adapts a function to a FailSink.
type FailSinkFunc func(ctx context.Context, f Fail) error

// Publish calls the underlying function.
func (fn FailSinkFunc) Publish(ctx context.Context, f Fail) error { return fn(ctx, f) }

// EmitFails publishes every detected fail to the sink, stopping on the first
// publish error (the bus contract: a publish failure is transient and the caller
// retries). Returns the number published.
func EmitFails(ctx context.Context, sink FailSink, fails []Fail) (int, error) {
	for i, f := range fails {
		if err := sink.Publish(ctx, f); err != nil {
			return i, err
		}
	}
	return len(fails), nil
}

// daysBetween returns the whole number of calendar days from start to end
// (negative if end precedes start), measured in UTC.
func daysBetween(start, end time.Time) int {
	return int(end.UTC().Sub(start.UTC()).Hours() / 24)
}
