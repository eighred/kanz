// Package markfeed folds the market price spine into a live mark source for
// tv-sync's unrealized-P&L projection (M3.5). It is a bus.EventHandler over
// market.v1.MarketDataEvent (the same normalized price events the market-data
// service publishes and the Binance ticker feed produces) and implements the
// projection's MarkSource seam structurally — so tv-sync stays a pure
// projection: prices arrive as FACTs, never a direct exchange call.
package markfeed

import (
	"context"
	"math/big"
	"sync"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/services/tv-sync/internal/dec"
)

// BusMarkSource holds the latest mark per instrument, folded from the price
// spine. Safe for concurrent folds (Handle) and reads (Mark).
type BusMarkSource struct {
	mu     sync.RWMutex
	prices map[string]*big.Rat
}

// New returns an empty mark source.
func New() *BusMarkSource { return &BusMarkSource{prices: make(map[string]*big.Rat)} }

// Mark returns the latest mark for an instrument, or nil when none is known
// (the projection then omits unrealized P&L — degrade, don't fabricate).
func (m *BusMarkSource) Mark(instrument string) *big.Rat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.prices[instrument]; ok {
		return new(big.Rat).Set(p)
	}
	return nil
}

// Handle is the bus.EventHandler: it folds one MarketDataEvent's price. A Trade
// updates the mark to the last trade price; a Quote to the bid/ask mid. Other
// payloads (Bar) are ignored. Malformed events are acked — a monitor never
// wedges the partition.
func (m *BusMarkSource) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &ev) != nil || ev.GetInstrumentId() == "" {
		return nil
	}
	var price *big.Rat
	switch {
	case ev.GetTrade() != nil && ev.GetTrade().GetPrice() != nil:
		price = dec.FromProto(ev.GetTrade().GetPrice())
	case ev.GetQuote() != nil && ev.GetQuote().GetBidPrice() != nil && ev.GetQuote().GetAskPrice() != nil:
		mid := new(big.Rat).Add(dec.FromProto(ev.GetQuote().GetBidPrice()), dec.FromProto(ev.GetQuote().GetAskPrice()))
		price = mid.Quo(mid, big.NewRat(2, 1))
	default:
		return nil
	}
	if price == nil || price.Sign() <= 0 {
		return nil
	}
	m.mu.Lock()
	m.prices[ev.GetInstrumentId()] = price
	m.mu.Unlock()
	return nil
}
