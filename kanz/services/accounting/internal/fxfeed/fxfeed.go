// Package fxfeed is the accounting service's live FX-rate surface for
// multi-currency NAV (WIRE-01d). ComputeNAVInCurrency needs an FXConverter — a
// point-in-time rate per foreign currency — but the FXTable was previously built
// only from a rate table supplied on the NAV request. This folds market.v1 FX
// quotes off the spine into a latest-rate cache and mints an FXConverter from it,
// so the NAV endpoint values a multi-currency book with no `fx` in the request.
//
// Like the risk-engine calibration cache (livequote), it does NOT import the
// market-data service's feed.Snapshot: that type is service-internal (the Go
// internal rule forbids the cross-service import), so the accounting service
// subscribes the market FX subjects itself and folds them here.
package fxfeed

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"

	accounting "github.com/kanz-eng/kanz/services/accounting/internal"
	"github.com/kanz-eng/kanz/services/accounting/internal/dec"
)

// LiveFX is the latest-rate cache: for each configured FX instrument it records
// the most recent mid as units of the reporting currency per 1 unit of the
// instrument's foreign currency. Converter mints a point-in-time
// accounting.FXConverter snapshot from it. Concurrency-safe; the reporting
// currency is implicitly 1 and never cached.
type LiveFX struct {
	reporting string
	// pairCcy maps an FX instrument_id (e.g. "EURUSD") to the FOREIGN currency it
	// quotes into the reporting currency (e.g. "EUR"). Reference metadata the
	// composition root supplies — which pair prices which currency, and in which
	// direction (the configured pair is assumed quoted foreign→reporting, so its
	// price is reporting-per-foreign directly).
	pairCcy map[string]string

	mu    sync.RWMutex
	rates map[string]*big.Rat // foreign currency -> reporting per 1 unit
}

// New builds a cache quoting into reporting for the configured pairs (FX
// instrument_id → foreign currency). Pairs is copied.
func New(reporting string, pairs map[string]string) *LiveFX {
	cp := make(map[string]string, len(pairs))
	for k, v := range pairs {
		cp[k] = v
	}
	return &LiveFX{reporting: reporting, pairCcy: cp, rates: map[string]*big.Rat{}}
}

// Handler is the bus.EventHandler the composition root subscribes to the market
// FX subjects. It decodes a market.v1.MarketDataEvent, and if the event's
// instrument is a configured FX pair with a usable mid, records the rate for its
// foreign currency. Events for unconfigured instruments are ignored (the same
// stream may carry equities); a malformed payload is returned (nack/DLQ) so
// market-data corruption surfaces loudly.
func (l *LiveFX) Handler(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("fxfeed: %s unmarshal: %w", env.GetEventType(), err)
	}
	ccy, ok := l.pairCcy[ev.GetInstrumentId()]
	if !ok {
		return nil
	}
	rate, ok := midRat(&ev)
	if !ok || rate.Sign() <= 0 {
		return nil // no usable mid; keep the prior rate serving
	}
	l.mu.Lock()
	l.rates[ccy] = rate
	l.mu.Unlock()
	return nil
}

// Converter mints a point-in-time FXConverter from the rates seen so far. The
// snapshot is immutable — a later tick updates the cache, not an already-issued
// converter — so a single NAV valuation sees one consistent rate set.
func (l *LiveFX) Converter() accounting.FXConverter {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return accounting.NewFXTable(l.reporting, l.rates)
}

// midRat extracts the exact mid rate from an FX market event: a quote's bid/ask
// mid, else a trade's last, else a bar's close. Exactness is preserved via
// big.Rat (FX rates flow into exact NAV math — no float). ok=false when the
// event carries no usable price.
func midRat(ev *marketpb.MarketDataEvent) (*big.Rat, bool) {
	switch d := ev.GetData().(type) {
	case *marketpb.MarketDataEvent_Quote:
		bid, bok := ratOf(d.Quote.GetBidPrice())
		ask, aok := ratOf(d.Quote.GetAskPrice())
		switch {
		case bok && aok:
			return new(big.Rat).Quo(new(big.Rat).Add(bid, ask), big.NewRat(2, 1)), true
		case bok:
			return bid, true
		case aok:
			return ask, true
		}
		return nil, false
	case *marketpb.MarketDataEvent_Trade:
		return ratOf(d.Trade.GetPrice())
	case *marketpb.MarketDataEvent_Bar:
		return ratOf(d.Bar.GetClose())
	default:
		return nil, false
	}
}

// ratOf converts a common.v1.Decimal to an exact rational; ok=false for nil.
func ratOf(d *commonpb.Decimal) (*big.Rat, bool) {
	if d == nil {
		return nil, false
	}
	return dec.FromProto(d), true
}

// ParsePairs parses the FX-pair reference spec — comma-separated
// `<instrument_id>:<foreign_currency>` entries (e.g. "EURUSD:EUR, GBPUSD:GBP") —
// into the instrument→foreign-currency map. An empty spec yields nil (live FX
// stays disabled). A malformed entry is a hard error — a mistyped pair map must
// fail loud at startup, not silently drop a currency from valuation.
func ParsePairs(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("fxfeed: FX pair %q: want instrument:foreign_currency", entry)
		}
		id, ccy := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if id == "" || ccy == "" {
			return nil, fmt.Errorf("fxfeed: FX pair %q: empty instrument or currency", entry)
		}
		out[id] = ccy
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
