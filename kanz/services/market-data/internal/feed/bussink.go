package feed

import (
	"context"
	"errors"
	"fmt"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Bus-backed Sink (WIRE-01a): the concrete Sink that stamps + publishes each
// normalized market.v1.MarketDataEvent onto the live spine via a shared
// bus.Producer, closing the PARITY-01a "the composition root binds the Sink to a
// bus producer" seam. Before this, the whole feed pipeline (adapters →
// normalizer → Gate → Snapshot) produced events that reached no consumer; the
// market-data SERVICE only consumed events it assumed already existed. This is
// the publisher that puts them there.
//
// It wraps a shared bus.Producer — the RISK-10 / posttrade.BusFailSink stance —
// so producer_sequence stays monotonic per (event_type, instrument). Publish is
// synchronous (the feed.Sink contract), so a slow broker backpressures the
// adapter rather than letting it unbounded-buffer. The Gate (PARITY-01g) and
// Partitioned (PARITY-05b) sinks decorate this without change: they are Sinks
// wrapping a Sink, and this is the innermost one.
type BusSink struct {
	producer   Publisher
	assetClass string
}

// Publisher is the one method BusSink needs from a producer.
//
// It is an INTERFACE rather than *bus.Producer so that the feed's publishes can
// be routed through bus.HealthPublisher, which counts consecutive failures and
// is what feeds /readyz (#299). A concrete producer type here would have meant
// the health wrapper could not sit in the path at all, and the readiness signal
// would have had to be re-derived from something further out — which is how a
// health check ends up watching a proxy for the thing it cares about instead of
// the thing itself.
//
// *bus.Producer and *bus.HealthPublisher both satisfy it.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

const (
	domainMarket            = "market"
	schemaRefMarketData     = "market.v1.MarketDataEvent:1"
	schemaVersionMarket     = 1
	defaultMarketAssetClass = "data"
)

// NewBusSink builds a bus-backed Sink over producer. assetClass is the middle
// segment of the emitted event_type (`market.<assetClass>.<variant>`, e.g.
// "equity" → market.equity.trade) — an equity feed sets "equity", an FX feed
// "fx"; empty defaults to "data". Errors when producer is nil.
func NewBusSink(producer Publisher, assetClass string) (*BusSink, error) {
	if producer == nil {
		return nil, errors.New("feed: nil producer")
	}
	if assetClass == "" {
		assetClass = defaultMarketAssetClass
	}
	return &BusSink{producer: producer, assetClass: assetClass}, nil
}

var _ Sink = (*BusSink)(nil)

// Publish emits ev as a market.v1.MarketDataEvent FACT, partitioned by
// instrument_id so an instrument's ticks stay ordered (the per-partition
// ordering invariant the whole feed path preserves). The envelope event_time is
// the tick's own event_time (not wall-clock), so bitemporal knowledge_time vs
// event_time stays honest downstream. IdempotencyKey is left empty — the
// producer stamps it to event_id for a FACT. A malformed event is rejected here
// (defense in depth behind the normalizer/Gate) rather than published.
func (s *BusSink) Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error {
	if err := Validate(ev); err != nil {
		return err
	}
	eventType := s.eventType(ev)
	return s.producer.Publish(ctx, bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionMarket,
		Domain:           domainMarket,
		EventTime:        ev.GetEventTime().AsTime(),
		PartitionKey:     ev.GetInstrumentId(),
		PayloadSchemaRef: schemaRefMarketData,
		Payload:          ev,
	})
}

// eventType maps a market variant to its `market.<assetClass>.<variant>` subject.
func (s *BusSink) eventType(ev *marketpb.MarketDataEvent) string {
	return fmt.Sprintf("%s.%s.%s", domainMarket, s.assetClass, variant(ev))
}

// variant is the event_type suffix for the event's oneof data variant.
func variant(ev *marketpb.MarketDataEvent) string {
	switch ev.GetData().(type) {
	case *marketpb.MarketDataEvent_Trade:
		return "trade"
	case *marketpb.MarketDataEvent_Quote:
		return "quote"
	case *marketpb.MarketDataEvent_Bar:
		return "bar"
	default:
		return "unknown"
	}
}
