package translate

import (
	"context"
	"errors"
	"math/big"
)

// The translator depends on live platform state through these narrow seams. In
// production the composition root binds them to the delivered projections
// (market-data for price, the accounting NAV projection for equity, the OMS
// position projection for CLOSE); in simulation and tests the static in-memory
// implementations here stand in — the "in-memory default is the test seam" rule.
//
// They live here rather than in webhook-ingest because BOTH brains resolve sizes
// against the same live state: an external TradingView signal and a native alpha
// signal must size identically, or the same intent would trade differently
// depending on which brain produced it.

// PriceSource returns the latest mark for an instrument as an exact rational.
// Needed to resolve PCT_OF_EQUITY and QUOTE_NOTIONAL sizes into a quantity.
type PriceSource interface {
	Price(ctx context.Context, instrumentID string) (*big.Rat, error)
}

// EquitySource returns a fund's net asset value (NAV) — the live accounting
// projection in production. Needed to resolve PCT_OF_EQUITY.
type EquitySource interface {
	Equity(ctx context.Context, fundID string) (*big.Rat, error)
}

// PositionSource returns a fund's signed net position in an instrument at a venue
// (positive long, negative short) — the OMS position projection in production.
// Needed to size a CLOSE (flatten).
type PositionSource interface {
	Position(ctx context.Context, fundID, venue, instrumentID string) (*big.Rat, error)
}

// VenueAllocation is one leg of a fund's fan-out: the venue (ISO 10383 MIC) and
// the fraction of the resolved size routed to it.
type VenueAllocation struct {
	Venue  string
	Weight *big.Rat
}

// AllocationPolicy maps a fund to the venues its signals fan out across, each
// with a weight applied to the resolved size. The map is immutable after
// construction.
type AllocationPolicy interface {
	VenuesFor(fundID string) ([]VenueAllocation, error)
}

// ErrNoAllocation is returned when a fund has no configured venue allocation —
// deny-by-default: an unmapped fund cannot trade.
var ErrNoAllocation = errors.New("translate: fund has no venue allocation")

// --- static in-memory implementations (sim/test defaults) ---

// StaticPrices is a fixed instrument→price map (rationals).
type StaticPrices map[string]*big.Rat

func (p StaticPrices) Price(_ context.Context, instrumentID string) (*big.Rat, error) {
	r, ok := p[instrumentID]
	if !ok {
		return nil, errors.New("translate: no price for " + instrumentID)
	}
	return new(big.Rat).Set(r), nil
}

// StaticEquity is a fixed fund→NAV map.
type StaticEquity map[string]*big.Rat

func (e StaticEquity) Equity(_ context.Context, fundID string) (*big.Rat, error) {
	r, ok := e[fundID]
	if !ok {
		return nil, errors.New("translate: no equity for " + fundID)
	}
	return new(big.Rat).Set(r), nil
}

// StaticPositions is a fixed (fund|venue|instrument)→signed-qty map, keyed
// "fund/venue/instrument".
type StaticPositions map[string]*big.Rat

func (p StaticPositions) Position(_ context.Context, fundID, venue, instrumentID string) (*big.Rat, error) {
	if r, ok := p[fundID+"/"+venue+"/"+instrumentID]; ok {
		return new(big.Rat).Set(r), nil
	}
	return new(big.Rat), nil // flat by default
}

// StaticAllocation is a fixed fund→venues map.
type StaticAllocation map[string][]VenueAllocation

func (a StaticAllocation) VenuesFor(fundID string) ([]VenueAllocation, error) {
	v, ok := a[fundID]
	if !ok || len(v) == 0 {
		return nil, ErrNoAllocation
	}
	return v, nil
}
