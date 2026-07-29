package audit

import (
	"fmt"
	"strings"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/auth"
)

// classification is the projected interpretation of one event: its audit Kind, a
// human Summary, and structured Attributes for query/report.
type classification struct {
	kind    Kind
	summary string
	attrs   map[string]string
}

// classify maps an envelope + payload to its audit interpretation. Recognized
// payloads are decoded and enriched; an unrecognized or undecodable event still
// yields a generic record (audit records EVERYTHING — a dropped event is an
// audit gap). Order matters: more specific matchers win.
//
// Decode-failure stance: a payload that fails to unmarshal is NOT an error here.
// The event is still recorded generically; the projector never drops an event
// because its payload was malformed (that itself is auditable).
func classify(env *envelopepb.Envelope, payload []byte) classification {
	class := env.GetEventClass()

	// 1) AUTH-01d authz decision — a DecisionLog on the platform authz subject.
	if env.GetEventType() == auth.AuthzDecisionEventType {
		if dl := decodeDecision(payload); dl != nil {
			return classification{KindAuthzDecision, dl.GetSummary(), dl.GetAttributes()}
		}
	}

	// 2) Data-quality event — OBSERVATION on the data domain (DATA-07).
	if class == envelopepb.EventClass_EVENT_CLASS_OBSERVATION && env.GetDomain() == "data" {
		if dqe := decodeDataQuality(payload); dqe != nil {
			return classification{KindDataQuality, dqe.GetSummary(), dataQualityAttrs(dqe)}
		}
	}

	// 3) Command outcome — the FACT every command emits (event-class-rules §2).
	if class == envelopepb.EventClass_EVENT_CLASS_FACT && strings.HasSuffix(env.GetEventType(), ".outcome") {
		if co := decodeOutcome(payload); co != nil && co.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_UNSPECIFIED {
			return classification{KindCommandOutcome, co.GetReason(), outcomeAttrs(co)}
		}
	}

	// 4) Command — any COMMAND-class event (the intent; its outcome is #3).
	if class == envelopepb.EventClass_EVENT_CLASS_COMMAND {
		return classification{KindCommand, fmt.Sprintf("command %s", env.GetEventType()), nil}
	}

	// 5) Model/strategy decision — a DecisionLog observation. Identified by the
	// envelope's payload_schema_ref (required field, EVT-16), NOT by blind
	// decode: a MetricObservation is wire-compatible with DecisionLog (both have
	// a string field 1), so decoding alone would misread a metric as a decision.
	if class == envelopepb.EventClass_EVENT_CLASS_OBSERVATION && schemaRefIs(env.GetPayloadSchemaRef(), "DecisionLog") {
		if dl := decodeDecision(payload); dl != nil && dl.GetDecider() != "" {
			attrs := cloneAttrs(dl.GetAttributes())
			if attrs == nil {
				attrs = map[string]string{}
			}
			attrs["decider"] = dl.GetDecider()
			return classification{KindDecision, dl.GetSummary(), attrs}
		}
	}

	// 6) Recorded but unrecognized — generic, envelope-level summary.
	return classification{KindEvent, fmt.Sprintf("%s %s", className(class), env.GetEventType()), nil}
}

func decodeDecision(b []byte) *observationpb.DecisionLog {
	var m observationpb.DecisionLog
	if proto.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

func decodeDataQuality(b []byte) *observationpb.DataQualityEvent {
	var m observationpb.DataQualityEvent
	if proto.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

func decodeOutcome(b []byte) *commandpb.CommandOutcome {
	var m commandpb.CommandOutcome
	if proto.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

func dataQualityAttrs(d *observationpb.DataQualityEvent) map[string]string {
	a := map[string]string{
		"subject":  d.GetSubject(),
		"severity": strings.TrimPrefix(d.GetSeverity().String(), "SEVERITY_"),
	}
	if pk := d.GetPartitionKey(); pk != "" {
		a["partition_key"] = pk
	}
	return a
}

func outcomeAttrs(c *commandpb.CommandOutcome) map[string]string {
	a := map[string]string{
		"status": strings.TrimPrefix(c.GetStatus().String(), "COMMAND_OUTCOME_STATUS_"),
	}
	if ec := c.GetErrorCode(); ec != "" {
		a["error_code"] = ec
	}
	if rr := c.GetResultRef(); rr != "" {
		a["result_ref"] = rr
	}
	return a
}

func cloneAttrs(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// className renders the envelope EventClass without its enum prefix
// (EVENT_CLASS_FACT -> FACT).
func className(c envelopepb.EventClass) string {
	return strings.TrimPrefix(c.String(), "EVENT_CLASS_")
}

// schemaRefIs reports whether a payload_schema_ref ("<schema-id>:<version>")
// names the given message type, case-insensitively (e.g.
// "observation.v1.DecisionLog:1" matches "DecisionLog").
func schemaRefIs(ref, msg string) bool {
	return strings.Contains(strings.ToLower(ref), strings.ToLower(msg))
}
