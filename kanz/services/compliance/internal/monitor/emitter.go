package monitor

import (
	"context"
	"time"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// Bus is the publish surface — satisfied by *bus.Producer. Narrow so the monitor
// is testable with a fake that records emitted FACTs.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Emitter publishes the compliance.v1.ComplianceBreach FACT. It is FACT-grade
// (EVENT_CLASS_FACT), partitioned by portfolio_id so a portfolio's breach
// timeline is totally ordered, and carries the full ComplianceResult.
type Emitter struct{ b Bus }

// NewEmitter wraps a Bus.
func NewEmitter(b Bus) *Emitter { return &Emitter{b: b} }

// EmitBreach publishes a ComplianceBreach for the breached portfolio.
func (e *Emitter) EmitBreach(ctx context.Context, res *compliancepb.ComplianceResult, trigger compliancepb.BreachTrigger, detectedAt time.Time) error {
	breach := &compliancepb.ComplianceBreach{
		PortfolioId:    res.GetPortfolioId(),
		MandateId:      res.GetMandateId(),
		MandateVersion: res.GetMandateVersion(),
		Result:         res,
		Trigger:        trigger,
		DetectedAt:     timestamppb.New(detectedAt.UTC()),
	}
	return e.b.Publish(ctx, bus.Event{
		Subject:          comp.SubjectBreach,
		EventType:        comp.EventTypeBreach,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           comp.Domain,
		EventTime:        detectedAt,
		PartitionKey:     res.GetPortfolioId(),
		PayloadSchemaRef: "compliance.v1.ComplianceBreach:1",
		Payload:          breach,
	})
}
