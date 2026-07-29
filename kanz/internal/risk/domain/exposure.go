package domain

import (
	"sort"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// ExposureDimension names a way exposure values are bucketed. The
// catalog is closed for v1 — adding a dimension is a domain change
// reviewed alongside the corresponding RISK-06 computation. New
// dimensions take the next available constant; existing values are
// never repurposed (mirrors the envelope-policy §4 discipline).
type ExposureDimension string

const (
	// ExposureByInstrument is the per-instrument bucket — the
	// canonical level RISK-06's compute layer derives others from.
	ExposureByInstrument ExposureDimension = "instrument"
	// ExposureByCurrency aggregates exposure by ISO 4217 currency.
	ExposureByCurrency ExposureDimension = "currency"
	// ExposureBySector aggregates by issuer industry sector.
	ExposureBySector ExposureDimension = "sector"
)

// Exposure is one bucketed exposure value. Gross is the sum of
// absolute exposures within the bucket; Net is the signed sum. Both
// are denominated in the portfolio's base currency at the time of
// computation; a multi-currency aggregator would carry per-currency
// Exposure values under ExposureByCurrency before rolling up.
type Exposure struct {
	Dimension ExposureDimension
	// Key identifies the bucket within Dimension — instrument id for
	// ExposureByInstrument, currency code for ExposureByCurrency, etc.
	Key string
	// Gross is the sum of absolute exposures; never negative.
	Gross *commonpb.Money
	// Net is the signed sum (long − short).
	Net *commonpb.Money
}

// ExposureSet is the concrete v1.ExposureSet — the api/v1 surface
// returns *ExposureSet so callers see only AsOf() through the
// interface, but the engine (and tests inside the risk module) can
// reach the full shape through type assertion or direct construction.
type ExposureSet struct {
	portfolioID v1.PortfolioID
	asOf        time.Time
	items       []Exposure
}

// NewExposureSet copies the items slice so the returned ExposureSet
// is immutable from the caller's perspective.
func NewExposureSet(id v1.PortfolioID, asOf time.Time, items []Exposure) *ExposureSet {
	cp := make([]Exposure, len(items))
	copy(cp, items)
	return &ExposureSet{portfolioID: id, asOf: asOf, items: cp}
}

// AsOf satisfies v1.ExposureSet.
func (s *ExposureSet) AsOf() time.Time { return s.asOf }

// PortfolioID returns the portfolio the exposure summarizes.
func (s *ExposureSet) PortfolioID() v1.PortfolioID { return s.portfolioID }

// Items returns a stable copy of the contained exposures, sorted by
// (dimension, key) for deterministic iteration.
func (s *ExposureSet) Items() []Exposure {
	out := make([]Exposure, len(s.items))
	copy(out, s.items)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dimension != out[j].Dimension {
			return out[i].Dimension < out[j].Dimension
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ByDimension returns the exposures in the named dimension. Used by
// RISK-06's aggregation logic and by scenario tooling.
func (s *ExposureSet) ByDimension(d ExposureDimension) []Exposure {
	out := make([]Exposure, 0, len(s.items))
	for _, e := range s.items {
		if e.Dimension == d {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Compile-time interface assertion — the test that ensures the
// concrete domain type still satisfies the api/v1 contract after any
// refactor. Cheaper and stricter than a runtime test.
var _ v1.ExposureSet = (*ExposureSet)(nil)
