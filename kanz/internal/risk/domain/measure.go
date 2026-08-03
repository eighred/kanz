package domain

import (
	"sort"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// MeasureSet is the concrete v1.MeasureSet — owned by RISK-07's
// compute layer, returned through api/v1's Engine.Measures and
// EvaluateScenario. The individual Measure value type lives in
// api/v1 (v1.Measure) because every caller of the measure surface
// needs to read its fields directly; the SET shape is domain-owned
// so RISK-07 can swap representations (map vs flat slice, indexed
// vs lazy) without churning the api/v1 contract.
type MeasureSet struct {
	portfolioID v1.PortfolioID
	asOf        time.Time
	measures    map[v1.MeasureName]v1.Measure
	// excluded are the positions no measure in this set includes,
	// because their currency is not the portfolio's base (#257). It
	// rides with the values rather than being recomputed by each reader
	// so that a set served from the degraded cache — where the portfolio
	// is long gone — still reports honestly what it left out, exactly as
	// it carries its own asOf.
	excluded []v1.CurrencyExclusion
}

// MeasureSetOption customizes a MeasureSet at construction.
type MeasureSetOption func(*MeasureSet)

// WithCurrencyExclusions records the positions excluded from every
// measure in the set (Portfolio.CurrencyExclusions). Omitting it means
// "the whole book was measured" — correct for a hand-built set in a test
// and for any set built from a single-currency portfolio.
func WithCurrencyExclusions(ex []v1.CurrencyExclusion) MeasureSetOption {
	return func(s *MeasureSet) {
		if len(ex) == 0 {
			// Nothing excluded is the overwhelmingly common case (a
			// single-currency book). Keep it allocation-free.
			s.excluded = nil
			return
		}
		s.excluded = append([]v1.CurrencyExclusion(nil), ex...)
	}
}

// NewMeasureSet copies the input map so callers cannot mutate the
// internal storage after construction.
func NewMeasureSet(id v1.PortfolioID, asOf time.Time, measures map[v1.MeasureName]v1.Measure, opts ...MeasureSetOption) *MeasureSet {
	cp := make(map[v1.MeasureName]v1.Measure, len(measures))
	for k, v := range measures {
		cp[k] = v
	}
	s := &MeasureSet{portfolioID: id, asOf: asOf, measures: cp}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// AsOf satisfies v1.MeasureSet.
func (s *MeasureSet) AsOf() time.Time { return s.asOf }

// PortfolioID returns the portfolio the measures summarize.
func (s *MeasureSet) PortfolioID() v1.PortfolioID { return s.portfolioID }

// Lookup satisfies v1.MeasureSet — returns the named measure and
// true, or the zero Measure and false.
func (s *MeasureSet) Lookup(name v1.MeasureName) (v1.Measure, bool) {
	m, ok := s.measures[name]
	return m, ok
}

// CurrencyExclusions satisfies v1.MeasureSet — the positions absent
// from every measure in this set.
//
// The result is a read-only VIEW, not a copy: callers must not mutate or
// re-order it. Same contract as Portfolio.Positions, and for the same
// reason — the engine calls this on every measures query just to ask
// whether the set is partial, and a defensive copy there would put a
// per-query allocation on the hot path that LATENCY-01c cleared. The
// constructor already copied the caller's slice, so the storage is the
// set's own.
func (s *MeasureSet) CurrencyExclusions() []v1.CurrencyExclusion {
	return s.excluded
}

// Names returns the measure names in stable lexicographic order so
// callers iterating the set get deterministic output (Go map
// iteration order is randomized).
func (s *MeasureSet) Names() []v1.MeasureName {
	out := make([]v1.MeasureName, 0, len(s.measures))
	for n := range s.measures {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Compile-time interface assertion — see exposure.go for the
// rationale.
var _ v1.MeasureSet = (*MeasureSet)(nil)
