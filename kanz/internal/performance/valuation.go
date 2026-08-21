package performance

import (
	"context"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// Point-in-time portfolio valuation (PERF-01b). Returns are computed over a
// series of portfolio market values; this is where those values come from —
// holdings (from a PositionProvider seam) marked at the bitemporal store's
// point-in-time price. The store read is the no-future-leakage hinge: a value AT
// observation time `at` is marked with prices KNOWN by the knowledge horizon
// `asOf`, so a vendor restatement stamped after asOf is invisible and a
// historical performance number is reproducible (the MODEL-01i contract,
// PERF-01f).

// Holding is a position quantity held in a portfolio at a valuation date.
type Holding struct {
	InstrumentID string
	Quantity     float64
}

// PositionProvider returns a portfolio's holdings as of observation time `at`,
// read at knowledge horizon `asOf` (a holdings restatement known only after asOf
// must not be visible). The risk-engine state or a position-history store
// satisfies it; performance owns the seam rather than reaching into the risk
// module (RISK-02).
type PositionProvider interface {
	Holdings(ctx context.Context, portfolioID string, at, asOf time.Time) ([]Holding, error)
}

// FlowProvider returns the external cashflows (contributions/withdrawals) into a
// portfolio within (start, end].
type FlowProvider interface {
	Flows(ctx context.Context, portfolioID string, start, end time.Time) ([]Flow, error)
}

// Valuer marks a portfolio to a single market value, point-in-time.
type Valuer struct {
	store     store.Store
	positions PositionProvider
	kind      store.PriceKind
}

// NewValuer wires a Valuer over the marketdata store and a position provider.
// kind selects which price mark to value at (PriceKindUnspecified ⇒ any kind,
// the store's "latest of any" read).
func NewValuer(s store.Store, positions PositionProvider, kind store.PriceKind) *Valuer {
	return &Valuer{store: s, positions: positions, kind: kind}
}

// Value marks the portfolio's holdings at observation time `at`, using only
// prices known by the knowledge horizon `asOf`. An instrument with no point-in-
// time price contributes 0 (an unpriced lot is not a leak; the response layer
// can flag coverage). A zero asOf means the live (latest-knowledge) read.
func (v *Valuer) Value(ctx context.Context, portfolioID string, at, asOf time.Time) (float64, error) {
	holdings, err := v.positions.Holdings(ctx, portfolioID, at, asOf)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, h := range holdings {
		px, ok, err := v.priceAt(ctx, h.InstrumentID, at, asOf)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		total += h.Quantity * px
	}
	return total, nil
}

// priceAt reads the latest price for an instrument with ObservationTime ≤ at AND
// KnowledgeTime ≤ asOf — the bitemporal point-in-time mark (a later restatement
// is filtered out). Shares pointInTimePrice with the benchmark/return reads so
// the whole performance layer marks on one consistent point-in-time basis.
func (v *Valuer) priceAt(ctx context.Context, instrumentID string, at, asOf time.Time) (float64, bool, error) {
	return pointInTimePrice(ctx, v.store, instrumentID, v.kind, at, asOf)
}

// ValueSeries marks the portfolio at each date in dates (sorted ascending),
// each read at the knowledge horizon asOf — the input to a time-weighted return.
func (v *Valuer) ValueSeries(ctx context.Context, portfolioID string, dates []time.Time, asOf time.Time) ([]float64, error) {
	sorted := append([]time.Time(nil), dates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	out := make([]float64, len(sorted))
	for i, d := range sorted {
		val, err := v.Value(ctx, portfolioID, d, asOf)
		if err != nil {
			return nil, err
		}
		out[i] = val
	}
	return out, nil
}

// BuildSubPeriods assembles flow-aware time-weighted sub-periods from a valuation
// series and the external flows. dates are the valuation boundaries (ascending);
// values[i] is the portfolio value at dates[i]. A flow dated within
// (dates[i-1], dates[i]] is attributed to the START of sub-period i (the
// conservative TWR convention — the flow is invested for the whole interval), so
// the sub-period return neutralizes it. Fewer than two valuation points ⇒ no
// sub-periods.
func BuildSubPeriods(dates []time.Time, values []float64, flows []Flow) []SubPeriod {
	if len(dates) < 2 || len(values) != len(dates) {
		return nil
	}
	out := make([]SubPeriod, 0, len(dates)-1)
	for i := 1; i < len(dates); i++ {
		var flow float64
		for _, f := range flows {
			if f.Time.After(dates[i-1]) && !f.Time.After(dates[i]) {
				flow += f.Amount
			}
		}
		out = append(out, SubPeriod{BeginValue: values[i-1], Flow: flow, EndValue: values[i]})
	}
	return out
}
