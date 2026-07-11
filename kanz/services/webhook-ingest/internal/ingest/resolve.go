package ingest

import (
	"github.com/kanz-eng/kanz/internal/signal/translate"
)

// The live-state seams and the signal→orders fan-out moved to
// internal/signal/translate when the native alpha engines became the second brain
// producing signals: both must reach the venues through ONE path, or the OMS's
// pre-trade gates could drift away from a parallel one. webhook-ingest keeps only
// what is genuinely webhook-shaped — HMAC authentication, alert parsing, and the
// TradingView ticker→instrument resolution below.
//
// These aliases keep the composition root and the existing test suite pointing at
// the same names; there is one definition, in translate.

type (
	// VenueAllocation is one leg of a fund's fan-out.
	VenueAllocation = translate.VenueAllocation
	// AllocationPolicy maps a fund to the venues its signals fan out across.
	AllocationPolicy = translate.AllocationPolicy
	// PriceSource returns the latest mark for an instrument.
	PriceSource = translate.PriceSource
	// EquitySource returns a fund's NAV.
	EquitySource = translate.EquitySource
	// PositionSource returns a fund's signed net position at a venue.
	PositionSource = translate.PositionSource
	// Publisher is the bus publish surface.
	Publisher = translate.Publisher

	// StaticPrices is a fixed instrument→price map.
	StaticPrices = translate.StaticPrices
	// StaticEquity is a fixed fund→NAV map.
	StaticEquity = translate.StaticEquity
	// StaticPositions is a fixed (fund/venue/instrument)→signed-qty map.
	StaticPositions = translate.StaticPositions
	// StaticAllocation is a fixed fund→venues map.
	StaticAllocation = translate.StaticAllocation
)

// ErrNoAllocation is returned when a fund has no configured venue allocation —
// deny-by-default: an unmapped fund cannot trade.
var ErrNoAllocation = translate.ErrNoAllocation

// SymbolResolver maps a raw TradingView ticker to a canonical Kanz instrument.
// This one stays here: it is webhook-shaped. A native alpha engine already works
// in canonical instrument_ids and has no ticker to resolve.
type SymbolResolver interface {
	Resolve(symbol string) (instrumentID string, ok bool)
}

// StaticSymbols is a fixed symbol→instrument map.
type StaticSymbols map[string]string

func (s StaticSymbols) Resolve(symbol string) (string, bool) { v, ok := s[symbol]; return v, ok }
