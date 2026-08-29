package position

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"log/slog"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
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
	relay  *outbox.Relay
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
	// THE PROJECTOR NO LONGER PUBLISHES; THE RELAY DOES (#795).
	//
	// It builds its own relay over the store's own queue rather than being handed
	// one, and that keeps the constructor's shape — the composition root passes
	// the same three arguments it always did, and gains no lifecycle to forget.
	// In the durable deployment the queue is the SAME outbox table the order
	// store writes, so the relay the OMS already runs is the background drainer;
	// this one exists for the inline Flush, which is what keeps a position FACT
	// as prompt as the direct publish it replaces.
	relay, err := outbox.NewRelay(book.Outbox(), b, slog.Default())
	if err != nil {
		return nil, err
	}
	return &Projector{book: book, relay: relay, tenant: tenant}, nil
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
	// THE ENTITY THE FOLD IS ORDERED ON, and therefore the key both FACTs are
	// announced under: (tenant, portfolio, instrument) — exactly the advisory
	// lock the durable store holds from before its first write to its commit.
	//
	// IT IS NOT portfolio_id, WHICH IS WHAT THE DIRECT PUBLISH USED. The outbox
	// promises "records with the same partition key are published in id order",
	// and that holds only where a key's commits cannot interleave. Keyed by
	// portfolio, two fills in DIFFERENT instruments take DIFFERENT advisory locks,
	// interleave freely, and id order stops being commit order — reintroducing the
	// reordering one layer down from where #795 found it. Keyed by the instrument
	// entity, same key implies same lock implies serialized commits.
	//
	// It is also the granularity a CONSUMER folds at: risk state applies one
	// instrument's position at a time, so the coarser key was never buying
	// ordering anybody needed.
	key := subject.PositionFor(p.tenant, portfolioID, fill.GetInstrumentId())

	if _, err := p.book.Apply(ctx, portfolioID, fill, fill.GetExecutedAt().AsTime(),
		func(ctx context.Context, a *Applied) ([]outbox.Record, error) {
			return p.records(ctx, key, portfolioID, fill, a)
		}); err != nil {
		// Never announce a position we could not fold. A PositionState built from a failed
		// write is a number the risk engine, the compliance monitor and the pre-trade gate
		// would all believe. Returning the error nacks the fill, so the bus redelivers it.
		return err
	}

	// FLUSHED INLINE, for the same reason the order handler flushes: the record is
	// durable either way, and this only decides whether the FACT arrives now or on
	// the relay's next tick. A stalled key comes back as an error so the fill is
	// nacked rather than acked over a position nobody has been told about.
	_, err = p.relay.Flush(ctx, key)
	return err
}

// records renders ONE BOOK, ANNOUNCED TWICE (EXEC-M19a).
//
// The FUND-LEVEL position first: it is what the risk engine and the compliance
// monitor consume, and a fund's exposure does not care which exchange holds the
// BTC. Then the PER-VENUE holding: a CLOSE signal must flatten what EACH VENUE
// actually holds, and you cannot sell 1 BTC on Binance if it is sitting at OKX.
//
// BOTH RIDE ONE PARTITION KEY, so the relay publishes them in the order they
// were built — aggregate then venue, which is the order the direct publish used
// — and a reader of one is never ahead of the other. The venue SUBJECT is finer
// than the key, which is safe in the only direction that matters: same key
// implies same advisory lock, so the venue FACT inherits the aggregate's
// ordering guarantee rather than needing one of its own.
func (p *Projector) records(ctx context.Context, key, portfolioID string, fill *orderpb.Fill, a *Applied) ([]outbox.Record, error) {
	agg, err := p.record(ctx, key, subject.PositionFor(p.tenant, portfolioID, fill.GetInstrumentId()),
		positionEventChanged, a.Aggregate)
	if err != nil {
		return nil, err
	}
	venue, err := p.record(ctx, key,
		subject.VenuePositionFor(p.tenant, portfolioID, fill.GetVenue(), fill.GetInstrumentId()),
		subject.VenuePositionChanged, a.Venue)
	if err != nil {
		return nil, err
	}
	return []outbox.Record{agg, venue}, nil
}

// record captures one PositionState FACT for the outbox.
//
// The SUBJECT carries the entity (EXEC-M20) so the compacted POSITION stream keeps the
// current state of each holding and a booting consumer learns them all in one read. The
// event_type stays the taxonomy name — consumers dispatch on it, and it is what tells them
// whether this is the fund's position or one venue's.
//
// outbox.From resolves the tenant and the lineage off ctx exactly as
// bus.Producer.stamp would have, which is what keeps a relayed FACT traceable to
// the fill that caused it — the relay publishes from a ticker, long after this
// ctx is gone. It REFUSES an empty tenant, and that refusal rolls the fold back
// with it: a position the platform cannot announce is one it must not book.
func (p *Projector) record(ctx context.Context, key, subj, eventType string, st *domainpb.PositionState) (outbox.Record, error) {
	return outbox.From(ctx, bus.Event{
		Subject:          subj,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        st.GetAsOf().AsTime(),
		PartitionKey:     key,
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
	orderEventFilled          = fillfact.SubjectFilled
	orderEventPartiallyFilled = fillfact.SubjectPartiallyFilled
	// positionEventChanged mirrors risk's ingest.EventTypePositionChanged — the
	// subject the risk engine ingests PositionState FACTs on.
	positionEventChanged = "risk.position.changed"
)
