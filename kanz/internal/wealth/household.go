// Package wealth is the goals-based wealth & advisory layer (WEALTH-01) —
// household aggregation, goals-based analytics, model portfolios, and rebalancing
// proposals: Aladdin Wealth atop the institutional engine (ROI #40).
//
// # Outside the risk module — the risk engine consumes a virtual portfolio
//
// wealth lives at kanz/internal/wealth, OUTSIDE kanz/internal/risk, so the
// RISK-02 boundary forbids importing the risk impl packages. Householding
// therefore aggregates accounts into a VIRTUAL PORTFOLIO (an instrument→value
// book) and exposure vectors that the risk engine consumes through its api/v*
// surface at the composition root — the same "produce the input, don't reach in"
// stance optimization and alternatives take. The rebalance proposal (WEALTH-01d)
// DOES reuse internal/optimization directly (a peer outside risk, no boundary
// there) — one trade-list engine for both the PM and the advisor workflow.
//
// All values are float64 — a household's aggregate market value, weights, and a
// goal's probability-of-success are derived statistics (the EVT-14 rule; money is
// exact Decimal at the wire, lands as float at the analytics edge).
package wealth

import "sort"

// Holding is one position in an account, marked to the household's reporting
// currency. AssetClass groups holdings for the asset-class exposure view.
type Holding struct {
	InstrumentID string
	AssetClass   string
	MarketValue  float64
}

// Account is an investment account in a household — the leaf householding
// aggregates over. Cash is the un-invested balance (it carries no instrument but
// counts toward total value).
type Account struct {
	AccountID string
	Holdings  []Holding
	Cash      float64
}

// Household is the set of accounts advice is given at — the unit exposure and
// risk are aggregated to.
type Household struct {
	HouseholdID string
	Accounts    []Account
}

// VirtualPortfolio is the household's accounts collapsed into one book — the
// input the risk engine scores as if it were a single portfolio (WEALTH-01b). It
// carries the per-instrument aggregate market value (the virtual positions), the
// total value, and the derived weight + asset-class exposure views.
type VirtualPortfolio struct {
	HouseholdID string
	// Holdings is instrument id → aggregate market value across every account.
	Holdings map[string]float64
	// Cash is the household's total un-invested balance.
	Cash float64
	// TotalValue is Σ holdings + cash — the denominator for weights.
	TotalValue float64
	// assetClass is instrument id → its asset class, for the exposure view.
	assetClass map[string]string
}

// Aggregate collapses a household's accounts into a virtual portfolio: it sums
// each instrument's market value across accounts and totals the cash. The result
// is the single book the risk engine scores for a household-level risk view.
func Aggregate(h Household) VirtualPortfolio {
	vp := VirtualPortfolio{
		HouseholdID: h.HouseholdID,
		Holdings:    map[string]float64{},
		assetClass:  map[string]string{},
	}
	for _, acct := range h.Accounts {
		vp.Cash += acct.Cash
		vp.TotalValue += acct.Cash
		for _, hold := range acct.Holdings {
			vp.Holdings[hold.InstrumentID] += hold.MarketValue
			vp.TotalValue += hold.MarketValue
			if hold.AssetClass != "" {
				vp.assetClass[hold.InstrumentID] = hold.AssetClass
			}
		}
	}
	return vp
}

// Weights returns each instrument's share of total household value. A zero (or
// negative) total yields an empty map — weights are undefined, reported empty
// rather than as NaN/Inf (the degraded-to-zero discipline). Cash is excluded
// from the weight numerators but is in the denominator, so invested weights sum
// to (1 − cash share).
func (vp VirtualPortfolio) Weights() map[string]float64 {
	out := make(map[string]float64, len(vp.Holdings))
	if vp.TotalValue <= 0 {
		return out
	}
	for id, mv := range vp.Holdings {
		out[id] = mv / vp.TotalValue
	}
	return out
}

// AssetClassExposure returns each asset class's share of total household value —
// the household's allocation across equity/fixed-income/etc. Holdings with no
// asset class are grouped under "" (unclassified); cash is reported under
// "CASH". A zero total yields an empty map.
func (vp VirtualPortfolio) AssetClassExposure() map[string]float64 {
	out := map[string]float64{}
	if vp.TotalValue <= 0 {
		return out
	}
	for id, mv := range vp.Holdings {
		out[vp.assetClass[id]] += mv / vp.TotalValue
	}
	if vp.Cash != 0 {
		out["CASH"] += vp.Cash / vp.TotalValue
	}
	return out
}

// Instruments returns the virtual portfolio's instrument ids in deterministic
// (sorted) order — the universe a risk recompute / a rebalance iterates over.
func (vp VirtualPortfolio) Instruments() []string {
	out := make([]string, 0, len(vp.Holdings))
	for id := range vp.Holdings {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
