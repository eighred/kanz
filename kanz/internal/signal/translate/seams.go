package translate

import (
	"context"
	"errors"
	"fmt"
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
// the FRACTION of the resolved size routed to it — a number in (0,1] that sums to
// exactly 1 across the fund's legs. Not a percentage. See ValidateAllocation.
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

// AllocationSet is an AllocationPolicy that can enumerate every fund it will ever
// answer for, so translate.New can audit the weights AT STARTUP — a bad split is a
// misconfiguration, and a misconfiguration must surface before the first event,
// not as a silently multiplied order on every trade the fund makes.
//
// A policy that CANNOT enumerate (a future dynamic one) is not audited here and
// must call ValidateAllocation on its own rows before returning them.
type AllocationSet interface {
	AllocationPolicy
	Allocations() map[string][]VenueAllocation
}

// ValidateAllocation is the ONE weight check — startup config and the translator's
// constructor both call it, so there is one definition of what a valid split is.
//
// The failure it exists to stop: weights were parsed and never summed, and fanOut
// multiplied the resolved quantity by each one unconditionally. Legs written as
// whole percents — {"weight":"60"}, {"weight":"40"}, the natural mistake because
// every other percentage on this surface (size_type: pct_of_equity) is
// whole-percent — turned a 2 BTC signal into 120 BTC + 80 BTC of live orders, with
// no error at load and none at fan-out. A duplicated leg (0.6 / 0.6) is the quiet
// version: 1.2x intended exposure on every trade, forever (#240).
func ValidateAllocation(fundID string, legs []VenueAllocation) error {
	if len(legs) == 0 {
		return fmt.Errorf("%w: fund %s has an empty venue allocation", ErrBadAllocation, fundID)
	}
	seen := make(map[string]struct{}, len(legs))
	total := new(big.Rat)
	for _, l := range legs {
		if l.Venue == "" {
			return fmt.Errorf("%w: fund %s has an allocation leg with no venue", ErrBadAllocation, fundID)
		}
		if _, dup := seen[l.Venue]; dup {
			return fmt.Errorf("%w: fund %s lists venue %s twice — a duplicated leg is double exposure "+
				"at one venue, not a split", ErrBadAllocation, fundID, l.Venue)
		}
		seen[l.Venue] = struct{}{}
		if l.Weight == nil || l.Weight.Sign() <= 0 {
			return fmt.Errorf("%w: fund %s venue %s has weight %s — a weight must be a positive "+
				"fraction (a zero leg never trades; a negative one inverts the side)",
				ErrBadAllocation, fundID, l.Venue, ratOrNil(l.Weight))
		}
		total.Add(total, l.Weight)
	}
	if total.Cmp(oneRat) != 0 {
		return fmt.Errorf("%w: fund %s weights sum to %s, want exactly 1 — weights are FRACTIONS of "+
			"the resolved size, not percentages, so 60/40 must be written 0.6/0.4. As written, every "+
			"order this fund places is scaled by %s",
			ErrBadAllocation, fundID, total.RatString(), total.RatString())
	}
	return nil
}

func ratOrNil(r *big.Rat) string {
	if r == nil {
		return "unset"
	}
	return r.RatString()
}

// ErrNoAllocation is returned when a fund has no configured venue allocation —
// deny-by-default: an unmapped fund cannot trade.
var ErrNoAllocation = errors.New("translate: fund has no venue allocation")

// ErrBadAllocation is a STARTUP failure: the fund's venue weights are not a split.
// It is deliberately not a per-signal error — the config is wrong, and the service
// must refuse to run rather than mis-size every signal it receives.
var ErrBadAllocation = errors.New("translate: invalid venue allocation")

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

// Allocations satisfies AllocationSet: the static map knows every fund it will
// ever answer for, so translate.New audits all of its weights at construction.
func (a StaticAllocation) Allocations() map[string][]VenueAllocation { return a }
