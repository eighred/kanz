package signal

import (
	"strings"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"
)

// Classify normalizes a bus event into a Signal. It returns ok=false for events
// that are not operational signals (the common case — most traffic is business
// data the autopilot ignores), so the controller only ever sees actionable
// input. The DATA-07 DataQualityEvent is decoded for its precise kind; the other
// conditions are recognized by event_type convention.
func Classify(env *envelopepb.Envelope, payload []byte) (Signal, bool) {
	base := Signal{
		Source:        env.GetSource(),
		EventID:       env.GetEventId(),
		CorrelationID: env.GetCorrelationId(),
		TenantID:      env.GetTenantId(),
		Time:          env.GetEventTime().AsTime(),
	}

	// DATA-07: decode the DataQualityEvent for its exact kind + severity.
	if strings.Contains(env.GetPayloadSchemaRef(), "DataQualityEvent") {
		if s, ok := fromDataQuality(base, payload); ok {
			return s, true
		}
	}

	// The remaining conditions are published as their own event types.
	et := env.GetEventType()
	switch {
	case matches(et, "reconcile", "diverg"):
		return finish(base, KindReconcileDivergence, subjectOf(env), SeverityCritical, "reconciliation divergence"), true
	case matches(et, "slo", "burn"):
		return finish(base, KindSLOBurn, subjectOf(env), SeverityCritical, "SLO fast burn"), true
	case matches(et, "circuit", "open"), matches(et, "breaker", "open"):
		return finish(base, KindCircuitOpen, subjectOf(env), SeverityCritical, "inference circuit open"), true
	}
	return Signal{}, false
}

func fromDataQuality(base Signal, payload []byte) (Signal, bool) {
	var dqe observationpb.DataQualityEvent
	if err := proto.Unmarshal(payload, &dqe); err != nil {
		return Signal{}, false
	}
	sev := mapSeverity(dqe.GetSeverity())
	subject := dqe.GetSubject()
	switch {
	case dqe.GetGap() != nil:
		return finish(base, KindDataGap, subject, sev, dqe.GetSummary()), true
	case dqe.GetStaleness() != nil:
		return finish(base, KindStaleness, subject, sev, dqe.GetSummary()), true
	case dqe.GetDrift() != nil:
		return finish(base, KindDrift, subject, sev, dqe.GetSummary()), true
	}
	return Signal{}, false
}

func mapSeverity(s observationpb.Severity) Severity {
	switch s {
	case observationpb.Severity_SEVERITY_CRITICAL:
		return SeverityCritical
	case observationpb.Severity_SEVERITY_WARNING:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// subjectOf is the affected entity for the non-DQE signals — the partition key
// (instrument/service), falling back to the domain.
func subjectOf(env *envelopepb.Envelope) string {
	if k := env.GetPartitionKey(); k != "" {
		return k
	}
	return env.GetDomain()
}

func matches(eventType string, parts ...string) bool {
	et := strings.ToLower(eventType)
	for _, p := range parts {
		if !strings.Contains(et, p) {
			return false
		}
	}
	return true
}

func finish(base Signal, kind Kind, subject string, sev Severity, summary string) Signal {
	base.Kind = kind
	base.Subject = subject
	base.Severity = sev
	base.Summary = summary
	return base
}
