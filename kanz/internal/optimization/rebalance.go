package optimization

import (
	"context"
	"math"
	"sort"
	"time"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"

	"github.com/kanz-eng/kanz/internal/compliance"
)

// Rebalancing (OPT-01d): diff the optimized target weights against the current
// book into a MINIMAL, turnover/cost-aware trade list, and package it as a
// RebalanceProposal — PROPOSED for human approval (the AUTO-01 conservative
// stance; never auto-executed). Minimality: an instrument whose weight moves
// less than a threshold is left untouched (no dust trades), so the list is the
// smallest set of trades that reaches the target within tolerance.

// TradeSide is the direction of a proposed trade (mirrors
// optimization.v1.TradeSide).
type TradeSide int

const (
	// Buy increases the position.
	Buy TradeSide = iota + 1
	// Sell decreases it.
	Sell
)

// String renders the side.
func (s TradeSide) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return "UNSPECIFIED"
	}
}

// ProposedTrade is one leg of the rebalance trade list.
type ProposedTrade struct {
	InstrumentID  string
	Side          TradeSide
	CurrentWeight float64
	TargetWeight  float64
	Quantity      float64 // units to trade (sign-free; Side carries direction)
	Notional      float64 // |Δw|·NAV
}

// RebalanceProposal is the optimizer's output — target weights + the minimal
// trade list to reach them, plus the ex-ante stats and the mandate feasibility.
// The Go-native shape of optimization.v1.RebalanceProposal.
type RebalanceProposal struct {
	PortfolioID     string
	Objective       Objective
	Targets         map[string]float64
	Trades          []ProposedTrade
	ExpectedReturn  float64
	ExpectedRisk    float64
	Turnover        float64
	AsOf            time.Time
	MandateFeasible bool
	Violations      []string
}

// DefaultRebalanceThreshold is the minimum absolute weight change that warrants
// a trade — below it the move is dust and skipped (minimality).
const DefaultRebalanceThreshold = 0.005 // 50 bps

// Rebalance diffs current vs target weights into a minimal trade list. An
// instrument in either map is considered (a target of 0 sells a held name to
// zero; a current of 0 buys a new name). |Δw| ≤ threshold ⇒ no trade. Quantity =
// notional/price when a price is known (price ≤ 0 ⇒ quantity 0, notional still
// reported). One-way turnover = Σ|Δw|/2 over the emitted trades. Trades are
// sorted by instrument for deterministic output.
func Rebalance(portfolioID string, current, target map[string]float64, nav float64, prices map[string]float64, threshold float64, asOf time.Time) RebalanceProposal {
	if threshold <= 0 {
		threshold = DefaultRebalanceThreshold
	}
	ids := unionKeys(current, target)
	sort.Strings(ids)

	var trades []ProposedTrade
	var gross float64
	for _, id := range ids {
		cw, tw := current[id], target[id]
		dw := tw - cw
		if math.Abs(dw) <= threshold {
			continue
		}
		notional := math.Abs(dw) * nav
		qty := 0.0
		if p := prices[id]; p > 0 {
			qty = notional / p
		}
		side := Buy
		if dw < 0 {
			side = Sell
		}
		trades = append(trades, ProposedTrade{
			InstrumentID:  id,
			Side:          side,
			CurrentWeight: cw,
			TargetWeight:  tw,
			Quantity:      qty,
			Notional:      notional,
		})
		gross += math.Abs(dw)
	}
	return RebalanceProposal{
		PortfolioID:     portfolioID,
		Targets:         target,
		Trades:          trades,
		Turnover:        gross / 2,
		AsOf:            asOf,
		MandateFeasible: true,
	}
}

// Propose runs the full OPT-01 pipeline: optimize the targets, check them
// against the COMP-01 mandate (deny-by-default), and diff into a minimal trade
// list — the one call a PM workflow / the OPT-01e service drives. The mandate
// constraints are folded into the optimization box up front (a book you can't
// hold, you can't optimize into) AND the result is re-checked against the full
// mandate; an infeasible result is still returned (MandateFeasible=false +
// violations) so the PM sees why, never silently dropped.
func Propose(ctx context.Context, portfolioID string, in MarketInputs, obj Objective, cons *ConstraintSet,
	current map[string]float64, nav float64, prices map[string]float64, threshold float64,
	classifier compliance.Classifier, engine *compliance.Engine, mandate *compliancepb.Mandate, currency string, asOf time.Time) (RebalanceProposal, error) {

	res, err := Optimize(in, obj, cons)
	if err != nil {
		return RebalanceProposal{}, err
	}
	proposal := Rebalance(portfolioID, current, res.Weights, nav, prices, threshold, asOf)
	proposal.Objective = obj
	proposal.ExpectedReturn = res.ExpectedReturn
	proposal.ExpectedRisk = res.ExpectedRisk

	feasible, violations := CheckMandate(ctx, res.Weights, nav, currency, classifier, engine, mandate, asOf)
	proposal.MandateFeasible = feasible
	proposal.Violations = violations
	return proposal, nil
}

func unionKeys(a, b map[string]float64) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}
