package position

import (
	"context"
	"errors"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// Bus is the publish surface — satisfied by *bus.Producer.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Projector is the bus consumer that turns order fill FACTs into position
// FACTs. Subscribe Handle to the OrderFilled + OrderPartiallyFilled subjects;
// it folds each fill into the Book and publishes the resulting PositionState on
// positionEventChanged (the risk engine's existing input subject).
type Projector struct {
	book *Book
	bus  Bus
}

// NewProjector wires a Book to a Bus.
func NewProjector(book *Book, b Bus) (*Projector, error) {
	if book == nil || b == nil {
		return nil, errors.New("position: book and bus required")
	}
	return &Projector{book: book, bus: b}, nil
}

// Handle is the bus.EventHandler for fill FACTs.
func (p *Projector) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	fill, portfolioID, err := decodeFill(env.GetEventType(), payload)
	if err != nil {
		return err
	}
	if fill == nil {
		return nil // not a fill-bearing event; ack
	}
	st := p.book.Apply(portfolioID, fill, fill.GetExecutedAt().AsTime())
	return p.publish(ctx, st)
}

func (p *Projector) publish(ctx context.Context, st *domainpb.PositionState) error {
	return p.bus.Publish(ctx, bus.Event{
		Subject:          positionEventChanged,
		EventType:        positionEventChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        st.GetAsOf().AsTime(),
		PartitionKey:     st.GetPortfolioId(),
		PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload:          st,
	})
}

// decodeFill extracts the Fill and its portfolio from a fill-bearing FACT.
func decodeFill(eventType string, payload []byte) (*orderpb.Fill, string, error) {
	switch eventType {
	case orderEventFilled:
		var ev orderpb.OrderFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		return ev.GetFill(), ev.GetState().GetPortfolioId(), nil
	case orderEventPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		return ev.GetFill(), ev.GetState().GetPortfolioId(), nil
	default:
		return nil, "", nil
	}
}

// Subjects the projector consumes and produces, inlined as literals rather than
// imported from the order and risk/ingest packages — the projector stays
// dependency-light, and the RISK-02 boundary forbids reaching into risk
// internals for a constant (the same stance services/compliance takes for this
// subject). These are stable wire contracts; a change is a breaking bus change
// caught by the schema/contract tests, not a silent drift.
const (
	orderEventFilled          = "order.order.filled"
	orderEventPartiallyFilled = "order.order.partially_filled"
	// positionEventChanged mirrors risk's ingest.EventTypePositionChanged — the
	// subject the risk engine ingests PositionState FACTs on.
	positionEventChanged = "risk.position.changed"
)
