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
}

// NewMeasureSet copies the input map so callers cannot mutate the
// internal storage after construction.
func NewMeasureSet(id v1.PortfolioID, asOf time.Time, measures map[v1.MeasureName]v1.Measure) *MeasureSet {
	cp := make(map[v1.MeasureName]v1.Measure, len(measures))
	for k, v := range measures {
		cp[k] = v
	}
	return &MeasureSet{portfolioID: id, asOf: asOf, measures: cp}
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
