// Package livequote is the risk module's live latest-quote surface for model
// calibration (WIRE-01c). The PARITY-03a calibrators pull their quote set from a
// point-in-time QuoteSource; this package is the risk-side end of that seam — a
// latest-value cache fed from the market.v1 spine, adapted to
// curve.QuoteSource, driven on a cadence by internal/risk/pricing/schedule.
//
// It deliberately does NOT import the market-data service's feed.Snapshot: that
// type is service-internal, and the risk-module boundary (test/arch) forbids an
// outsider reaching into risk, so the reverse coupling cannot be the wiring. The
// composition root that owns the calibration loop is the risk-engine service
// (the boundary's designated composition root); it subscribes the market
// subjects and folds them here, exactly as market-data folds them into history.
package livequote

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
)

// LiveQuotes is a concurrency-safe last-value cache: the most-recent market
// event per CALIBRATION instrument. A calibration model does not consume the
// tick STREAM — it needs the latest quote for each calibration instrument as of
// now — so this is exactly the surface it reads (the market-data
// feed.Snapshot's risk-side mirror).
//
// THE ADMITTED KEY SPACE IS FIXED AT CONSTRUCTION, and that is the bound rather
// than a detail of it (#894). The handler is subscribed to the risk-engine's
// market subjects, whose default is the `market.>` WILDCARD, so what arrives
// here is the whole spine's instrument universe — every venue, every asset
// class, and every instrument the estate has ever published, including delisted
// ones. The only reader (SnapshotRateSource) asks for the configured strip and
// nothing else, so any other key this map held would be written, never read and
// never freed, for the life of the pod. Admitting only the configured universe
// makes the map's size a property of THIS POD'S CONFIGURATION rather than of the
// spine's history: it needs no evictor because the key space cannot grow.
//
// The subscription stays a wildcard deliberately. midPrice reads a Quote, a
// Trade or a Bar, so which subject a calibration instrument's price arrives on
// is not derivable from the instrument id; the filter belongs on the map, not on
// the subscription.
type LiveQuotes struct {
	mu     sync.RWMutex
	latest map[string]*marketpb.MarketDataEvent
	// wanted is the admitted instrument universe. Written once by New and never
	// mutated afterwards, so it is not part of the mutex's charge.
	wanted map[string]bool
}

// New returns an empty cache admitting exactly the instruments of the
// calibration universe — the SAME slice that binds the reader in
// NewSnapshotRateSource, so the two cannot drift into caching one set and
// reading another.
//
// An empty universe yields a cache that admits nothing, which is the fail-closed
// direction: a calibration loop with no configured strip produces no quotes
// either way, and the composition root refuses to start on an empty spec rather
// than running blind.
func New(instruments []RateInstrument) *LiveQuotes {
	wanted := make(map[string]bool, len(instruments))
	for _, in := range instruments {
		wanted[in.InstrumentID] = true
	}
	return &LiveQuotes{
		latest: make(map[string]*marketpb.MarketDataEvent, len(wanted)),
		wanted: wanted,
	}
}

// Update records ev as the latest for its instrument IF that instrument is in
// the configured universe. An event for anything else is dropped rather than
// cached: nothing reads it, and retaining it is the unbounded growth #894 found.
func (q *LiveQuotes) Update(ev *marketpb.MarketDataEvent) {
	if ev == nil {
		return
	}
	q.mu.Lock()
	if id := ev.GetInstrumentId(); q.wanted[id] {
		q.latest[id] = ev
	}
	q.mu.Unlock()
}

// Latest returns the most-recent event for an instrument, ok=false if none seen.
func (q *LiveQuotes) Latest(instrumentID string) (*marketpb.MarketDataEvent, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	ev, ok := q.latest[instrumentID]
	return ev, ok
}

// Universe returns the instrument ids this cache admits, sorted — the bound
// itself, read off the cache rather than re-derived by its caller. The
// composition root logs it at startup, so which instruments a pod retains quotes
// for is an operator-visible fact instead of something inferred from the
// wildcard subscription.
//
// It replaces the former Instruments(), which returned the CACHED set: that
// method had no caller anywhere in the module, and the only question it could
// answer that this one cannot — "what else has arrived on the wildcard?" — is
// exactly what no longer accumulates.
func (q *LiveQuotes) Universe() []string {
	out := make([]string, 0, len(q.wanted))
	for id := range q.wanted {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Handler is the bus.EventHandler value the risk-engine wires into
// bus.Consumer.Subscribe for the market quote subjects: it unmarshals a
// market.v1.MarketDataEvent and folds it into the cache. A malformed payload is
// returned (nack/DLQ) so market-data corruption surfaces loudly, the same stance
// as the market-data ingestor. The cache is a last-value write, so calibration
// quote loss is impossible short of a decode failure.
//
// An event for an instrument OUTSIDE the calibration universe decodes, is
// validated, and is then dropped by Update with a nil return — it is acked, not
// nacked. It is not this subscriber's message: on a `market.>` wildcard almost
// every event is one, and nacking them would DLQ the market spine rather than
// report anything.
func (q *LiveQuotes) Handler(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("livequote: %s unmarshal: %w", env.GetEventType(), err)
	}
	// Refused before Update caches it (#95): a quote held in the live cache is read
	// by every revaluation that follows, so an out-of-domain exponent admitted here
	// is not one bad message, it is every subsequent valuation of that instrument.
	if field, in := dec.InDomainDeep(&ev); !in {
		return fmt.Errorf("livequote: %s carries an out-of-domain exponent at %s", env.GetEventType(), field)
	}
	q.Update(&ev)
	return nil
}

// midPrice extracts the representative rate/price from a market event: a quote's
// bid/ask mid, else a trade's last, else a bar's close. ok=false when the event
// carries no usable price.
func midPrice(ev *marketpb.MarketDataEvent) (float64, bool) {
	switch d := ev.GetData().(type) {
	case *marketpb.MarketDataEvent_Quote:
		bid, bok := dec.Float64(d.Quote.GetBidPrice())
		ask, aok := dec.Float64(d.Quote.GetAskPrice())
		switch {
		case bok && aok:
			return (bid + ask) / 2, true
		case bok:
			return bid, true
		case aok:
			return ask, true
		}
		return 0, false
	case *marketpb.MarketDataEvent_Trade:
		return dec.Float64(d.Trade.GetPrice())
	case *marketpb.MarketDataEvent_Bar:
		return dec.Float64(d.Bar.GetClose())
	default:
		return 0, false
	}
}
