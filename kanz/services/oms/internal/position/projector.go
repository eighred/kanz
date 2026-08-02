package position

import (
	"context"
	"errors"
	"fmt"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
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
	book   Store
	bus    Bus
	tenant string
}

// NewProjector wires a position Store to a Bus.
//
// It takes the STORE, not the in-memory Book: what this publishes is the fund's ABSOLUTE
// position, and with `replicas: 2` a per-pod map publishes a fraction of it as though it
// were the whole (EXEC-M18).
func NewProjector(book Store, b Bus, tenant string) (*Projector, error) {
	if book == nil || b == nil {
		return nil, errors.New("position: store and bus required")
	}
	if tenant == "" {
		// The tenant is a TOKEN IN THE SUBJECT now (EXEC-M20). An empty one would put
		// every tenant's holdings on the same subject, where a compacted stream keeps
		// one and silently discards the rest.
		return nil, errors.New("position: tenant required — it is a token in the position subject")
	}
	return &Projector{book: book, bus: b, tenant: tenant}, nil
}

// Handle is the bus.EventHandler for fill FACTs.
func (p *Projector) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// The projector is the SECOND consumer of order.order.filled, and it folds into
	// a book scoped to p.tenant while publishing on subjects built from p.tenant —
	// so an envelope from another tenant would be folded into, and published as,
	// this tenant's position (#223).
	if err := bus.RequireTenantScope(env.GetTenantId(), p.tenant); err != nil {
		return err
	}
	fill, portfolioID, err := decodeFill(env.GetEventType(), payload)
	if err != nil {
		return err
	}
	if fill == nil {
		return nil // not a fill-bearing event; ack
	}
	applied, err := p.book.Apply(ctx, portfolioID, fill, fill.GetExecutedAt().AsTime())
	if err != nil {
		// Never publish a position we could not fold. A PositionState built from a failed
		// write is a number the risk engine, the compliance monitor and the pre-trade gate
		// would all believe. Returning the error nacks the fill, so the bus redelivers it.
		return err
	}

	// ONE BOOK, PUBLISHED TWICE (EXEC-M19a).
	//
	// The FUND-LEVEL position first: it is what the risk engine and the compliance monitor
	// consume, and a fund's exposure does not care which exchange holds the BTC.
	//
	// Then the PER-VENUE holding: a CLOSE signal must flatten what EACH VENUE actually
	// holds, and you cannot sell 1 BTC on Binance if it is sitting at OKX. The fill has
	// always carried the venue; until now the projector threw it away, and webhook-ingest's
	// CLOSE path was wired to an empty map because there was nothing to wire.
	//
	// The per-venue publish is NOT allowed to fail silently: a venue FACT that never lands
	// leaves the execution plane's book missing a holding, and a holding it cannot see is a
	// holding it will never close. Returning the error nacks the fill, and the aggregate is
	// idempotent on redelivery (the fold is claimed exactly once), so the retry is safe.
	if err := p.publish(ctx, subject.PositionFor(p.tenant, portfolioID, fill.GetInstrumentId()),
		positionEventChanged, applied.Aggregate); err != nil {
		return err
	}
	return p.publish(ctx,
		subject.VenuePositionFor(p.tenant, portfolioID, fill.GetVenue(), fill.GetInstrumentId()),
		subject.VenuePositionChanged, applied.Venue)
}

// publish emits one PositionState FACT.
//
// The SUBJECT carries the entity (EXEC-M20) so the compacted POSITION stream keeps the
// current state of each holding and a booting consumer learns them all in one read. The
// event_type stays the taxonomy name — consumers dispatch on it, and it is what tells them
// whether this is the fund's position or one venue's.
func (p *Projector) publish(ctx context.Context, subj, eventType string, st *domainpb.PositionState) error {
	return p.bus.Publish(ctx, bus.Event{
		Subject:          subj,
		EventType:        eventType,
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
//
// IT BOUNDS THE DECIMAL DOMAIN HERE, WHERE THE MESSAGE IS STILL WHOLE (#95).
// The Fill this returns is folded by book.go and postgres.go, which convert its
// price and quantity with the unbounded dec.FromProto. Decimal.exponent is an
// unvalidated wire field, so a fill carrying {1, 2000000000} would not book a
// wrong position — it would never return, holding the projector, and the
// execution book would stop advancing while the service still reported healthy.
// This is the last point at which the whole message is in one place to refuse.
func decodeFill(eventType string, payload []byte) (*orderpb.Fill, string, error) {
	switch eventType {
	case orderEventFilled:
		var ev orderpb.OrderFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		if field, in := dec.InDomainDeep(&ev); !in {
			return nil, "", fmt.Errorf("fill carries an out-of-domain exponent at %s", field)
		}
		return ev.GetFill(), ev.GetState().GetPortfolioId(), nil
	case orderEventPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		if field, in := dec.InDomainDeep(&ev); !in {
			return nil, "", fmt.Errorf("fill carries an out-of-domain exponent at %s", field)
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
