// Package audit is the COMP-01e compliance-decision recorder. Every compliance
// decision — pre-trade pass/reject and post-trade breach — is recorded as an
// observation.v1.DecisionLog and published on a durable observation subject,
// exactly the AUTH-01d authorization-audit pattern: the DecisionLog already
// models "what decided, the summary, the structured factors", which is what a
// compliance verdict is, and AUDIT-01's append-only hash-chain projection
// materializes it into the queryable, tamper-evident audit store.
//
// Recording is best-effort on the hot path (the pre-trade gate): a failure is
// logged loudly but never fails the decision — an audit-sink outage must not
// become a trading outage (the AUTH-01d stance). Durability is the AUDIT-01
// projection's responsibility, not the live stream's.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	// Decider identifies the compliance engine as the decider, "{type}:{id}".
	Decider = "compliance:engine"

	// DecisionDomain / DecisionEventType are the envelope fields AUDIT-01 selects
	// compliance decisions by off the observation stream.
	DecisionDomain    = "platform"
	DecisionEventType = "platform.compliance.decision"

	// SubjectDecision is the durable observation subject the decisions ride.
	SubjectDecision = "platform.compliance.decision"
)

// Bus is the publish surface — satisfied by *bus.Producer.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// BusRecorder publishes each compliance decision as a DecisionLog OBSERVATION.
type BusRecorder struct {
	b      Bus
	logger *slog.Logger
}

// NewBusRecorder wraps a Bus (logger defaults to slog.Default()).
func NewBusRecorder(b Bus, logger *slog.Logger) *BusRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &BusRecorder{b: b, logger: logger}
}

var _ comp.DecisionRecorder = (*BusRecorder)(nil)

// Record maps the decision to a DecisionLog and publishes it. A publish failure
// is returned so a caller that wants fail-closed semantics can react, but both
// enforcement points call Record best-effort (logging, not failing) per the
// AUTH-01d stance.
func (r *BusRecorder) Record(ctx context.Context, rec comp.DecisionRecord) error {
	log := BuildDecisionLog(rec)
	err := r.b.Publish(ctx, bus.Event{
		Subject:          SubjectDecision,
		EventType:        DecisionEventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
		SchemaVersion:    1,
		Domain:           DecisionDomain,
		PartitionKey:     rec.Result.GetPortfolioId(),
		PayloadSchemaRef: "observation.v1.DecisionLog:1",
		Payload:          log,
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "compliance decision record failed", "err", err,
			"portfolio_id", rec.Result.GetPortfolioId())
	}
	return err
}

// BuildDecisionLog maps a compliance DecisionRecord to a DecisionLog. Exported
// so the recorder and tests share one mapping (the "one mapping, not two" rule
// the RISK-10 / AUTH-01d converters follow).
func BuildDecisionLog(rec comp.DecisionRecord) *observationpb.DecisionLog {
	res := rec.Result
	verdict := strings.TrimPrefix(res.GetStatus().String(), "COMPLIANCE_STATUS_")
	attrs := map[string]string{
		"phase":           rec.Phase,
		"status":          verdict,
		"portfolio_id":    res.GetPortfolioId(),
		"mandate_id":      res.GetMandateId(),
		"mandate_version": strconv.FormatUint(res.GetMandateVersion(), 10),
		"violation_count": strconv.Itoa(len(res.GetViolations())),
	}
	if rec.Phase == comp.PhasePreTrade {
		attrs["allowed"] = strconv.FormatBool(rec.Allowed)
	}
	if rec.OrderID != "" {
		attrs["order_id"] = rec.OrderID
	}
	if rec.Issuer != "" {
		attrs["issuer"] = rec.Issuer
	}
	if rec.Trigger != "" {
		attrs["trigger"] = strings.TrimPrefix(rec.Trigger, "BREACH_TRIGGER_")
	}
	if v := firstBreach(res); v != nil {
		attrs["first_rule_id"] = v.GetRuleId()
		attrs["first_rule_type"] = strings.TrimPrefix(v.GetRuleType().String(), "RULE_TYPE_")
	}
	return &observationpb.DecisionLog{
		Decider:    Decider,
		Summary:    summary(rec, verdict),
		Attributes: attrs,
	}
}

func summary(rec comp.DecisionRecord, verdict string) string {
	res := rec.Result
	if rec.Phase == comp.PhasePreTrade {
		gate := "ADMIT"
		if !rec.Allowed {
			gate = "REJECT"
		}
		return fmt.Sprintf("pre-trade %s order %s on portfolio %s: %s (%d violations)",
			gate, rec.OrderID, res.GetPortfolioId(), verdict, len(res.GetViolations()))
	}
	return fmt.Sprintf("post-trade %s on portfolio %s: %s (%d violations)",
		verdict, res.GetPortfolioId(), verdict, len(res.GetViolations()))
}

func firstBreach(res *compliancepb.ComplianceResult) *compliancepb.Violation {
	for _, v := range res.GetViolations() {
		if v.GetSeverity() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
			return v
		}
	}
	if vs := res.GetViolations(); len(vs) > 0 {
		return vs[0]
	}
	return nil
}
