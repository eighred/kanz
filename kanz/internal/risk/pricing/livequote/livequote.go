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
	"math"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
)

// LiveQuotes is a concurrency-safe last-value cache: the most-recent market
// event per instrument. A calibration model does not consume the tick STREAM —
// it needs the latest quote for each calibration instrument as of now — so this
// is exactly the surface it reads (the market-data feed.Snapshot's risk-side
// mirror).
type LiveQuotes struct {
	mu     sync.RWMutex
	latest map[string]*marketpb.MarketDataEvent
}

// New returns an empty cache.
func New() *LiveQuotes {
	return &LiveQuotes{latest: map[string]*marketpb.MarketDataEvent{}}
}

// Update records ev as the latest for its instrument.
func (q *LiveQuotes) Update(ev *marketpb.MarketDataEvent) {
	if ev == nil {
		return
	}
	q.mu.Lock()
	q.latest[ev.GetInstrumentId()] = ev
	q.mu.Unlock()
}

// Latest returns the most-recent event for an instrument, ok=false if none seen.
func (q *LiveQuotes) Latest(instrumentID string) (*marketpb.MarketDataEvent, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	ev, ok := q.latest[instrumentID]
	return ev, ok
}

// Instruments returns the cached instruments, sorted.
func (q *LiveQuotes) Instruments() []string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]string, 0, len(q.latest))
	for id := range q.latest {
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
		bid, bok := decFloat(d.Quote.GetBidPrice())
		ask, aok := decFloat(d.Quote.GetAskPrice())
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
		return decFloat(d.Trade.GetPrice())
	case *marketpb.MarketDataEvent_Bar:
		return decFloat(d.Bar.GetClose())
	default:
		return 0, false
	}
}

// decFloat converts a common.v1.Decimal to float64 — the same edge conversion
// the risk compute + performance layers do (exact Decimal on the wire, float in
// the calibration math). ok=false for a nil decimal.
func decFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	return float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent())), true
}
