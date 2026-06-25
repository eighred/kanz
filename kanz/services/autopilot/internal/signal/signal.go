// Package signal is the normalized input to the autopilot control loop
// (AUTO-01a): the heterogeneous operational events the platform already emits —
// DATA-07 data-quality events (gap/staleness/drift), DATA-05 reconciliation
// divergence, model drift, SLO burn-rate, and inference circuit-breaker state —
// folded into one Signal shape the controller reasons over.
package signal

import "time"

// Kind is the operational condition a signal reports.
type Kind string

const (
	KindDataGap             Kind = "data_gap"             // DATA-01 sequence gap
	KindStaleness           Kind = "staleness"            // DATA-02 excessive lag
	KindDrift               Kind = "drift"                // DATA-04 / model input-distribution drift
	KindReconcileDivergence Kind = "reconcile_divergence" // DATA-05 NATS↔Kafka divergence
	KindSLOBurn             Kind = "slo_burn"             // OBS-01 SLO fast-burn
	KindCircuitOpen         Kind = "circuit_open"         // PRED-07 inference breaker tripped
)

// Severity is the normalized urgency, mapped from the source's own scale
// (observation.v1.Severity, alert severity, …).
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityWarning:
		return "warning"
	default:
		return "info"
	}
}

// Signal is one normalized operational event.
type Signal struct {
	// Kind is the condition class.
	Kind Kind
	// Subject is the affected entity the remediation targets — an instrument /
	// data subject (quarantine), a model id (rollback), a service (scale), a
	// region (failover). Its meaning is Kind-specific.
	Subject string
	// Severity is the normalized urgency.
	Severity Severity
	// Summary is the human-readable description from the source.
	Summary string
	// Source is the emitting service (envelope source).
	Source string
	// Attributes carries Kind-specific extra fields (e.g. model_id, region).
	Attributes map[string]string
	// EventID / CorrelationID / TenantID tie the signal back to the triggering
	// event for audit + lineage.
	EventID       string
	CorrelationID string
	TenantID      string
	// Time is the detection time (envelope event_time).
	Time time.Time
}

// Attr returns an attribute or "" if absent.
func (s Signal) Attr(key string) string {
	if s.Attributes == nil {
		return ""
	}
	return s.Attributes[key]
}
