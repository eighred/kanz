package performance

import (
	"context"
	"time"

	"github.com/kanz-eng/kanz/internal/marketdata/store"
)

// Benchmark plane (PERF-01c): a benchmark is a weighted set of constituents
// whose return is the weighted sum of constituent returns; portfolio-vs-
// benchmark active return is the headline relative number. Constituents +
// returns are sourced as reference/market data — the same point-in-time store
// the portfolio is valued against, so portfolio and benchmark are measured on
// one consistent price basis.

// Constituent is one weighted member of a benchmark, carrying its sector bucket
// for attribution.
type Constituent struct {
	InstrumentID string
	Weight       float64
	Sector       string // taxonomy:code bucket, "" ⇒ unclassified
}

// Benchmark is a weighted constituent set — the Go-native shape of
// performance.v1.BenchmarkDefinition.
type Benchmark struct {
	ID           string
	Currency     string
	Constituents []Constituent
}

// Return is the benchmark's return over a window given each constituent's
// return: Σ (wᵢ/Σw)·rᵢ. Weights are renormalized by their sum so a benchmark
// whose weights don't total exactly 1 (rounding, a dropped constituent) is still
// a proper weighted average. A constituent with no return contributes 0 weight.
func (b Benchmark) Return(constituentReturns map[string]float64) float64 {
	var weighted, totalWeight float64
	for _, c := range b.Constituents {
		r, ok := constituentReturns[c.InstrumentID]
		if !ok {
			continue
		}
		weighted += c.Weight * r
		totalWeight += c.Weight
	}
	if totalWeight == 0 {
		return 0
	}
	return weighted / totalWeight
}

// ActiveReturn is portfolioReturn − benchmarkReturn — the value the manager
// added (or lost) relative to the benchmark, the quantity PERF-01d attributes.
func ActiveReturn(portfolioReturn, benchmarkReturn float64) float64 {
	return portfolioReturn - benchmarkReturn
}

// InstrumentReturn computes a single instrument's point-in-time price return
// over (start, end], read at knowledge horizon asOf: P_end/P_start − 1 using only
// prices known by asOf. ok=false when either endpoint has no point-in-time
// price. This is the shared return source feeding both the benchmark constituent
// returns and the per-sector returns of attribution — one price basis, no future
// leakage (a restatement after asOf is invisible).
func InstrumentReturn(ctx context.Context, s store.Store, instrumentID string, kind store.PriceKind, start, end, asOf time.Time) (float64, bool, error) {
	pStart, ok, err := pointInTimePrice(ctx, s, instrumentID, kind, start, asOf)
	if err != nil || !ok || pStart == 0 {
		return 0, false, err
	}
	pEnd, ok, err := pointInTimePrice(ctx, s, instrumentID, kind, end, asOf)
	if err != nil || !ok {
		return 0, false, err
	}
	return pEnd/pStart - 1, true, nil
}

// pointInTimePrice is the bitemporal spot read shared by valuation + return: the
// latest price with ObservationTime ≤ at AND KnowledgeTime ≤ asOf.
func pointInTimePrice(ctx context.Context, s store.Store, instrumentID string, kind store.PriceKind, at, asOf time.Time) (float64, bool, error) {
	hist, err := s.History(ctx, store.Query{InstrumentID: instrumentID, Kind: kind, End: at, AsOf: asOf})
	if err != nil || len(hist) == 0 {
		return 0, false, err
	}
	return decimalToFloat(hist[len(hist)-1].Price), true, nil
}
