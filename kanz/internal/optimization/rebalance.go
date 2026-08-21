package optimization

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/internal/compliance"
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

// MandateStatus is the outcome of the COMP-01 mandate check on a proposal, in
// THREE states because two cannot hold the answer.
//
// The field used to be `MandateFeasible bool`, and a bool cannot tell "no
// mandate was ever consulted" from "consulted, and it passed". It defaulted to
// the second: Rebalance stamped every proposal it built as feasible, no caller
// on the service's route ever ran CheckMandate, and the bridge that turns a
// proposal into live order commands used that stamp as its only gate (#646).
// The zero value is now MandateUnchecked, so a proposal cannot acquire a verdict
// merely by being constructed, and ToOrders refuses anything that is not
// MandateFeasible.
type MandateStatus int

const (
	// MandateUnchecked is the ZERO VALUE: nothing has evaluated this proposal
	// against a mandate. It is not a soft pass — it materializes into no orders,
	// and the refusal names it.
	MandateUnchecked MandateStatus = iota
	// MandateFeasible means CheckMandate ran against a real mandate and found no
	// BREACH. It is the ONLY state that may become order commands.
	MandateFeasible
	// MandateInfeasible means CheckMandate ran and found at least one BREACH;
	// Violations carries the reasons. The proposal is still returned so the PM
	// sees why, never silently dropped.
	MandateInfeasible
)

// String renders the verdict. An out-of-range value renders as itself rather
// than falling back to one of the three: a corrupt verdict that printed as
// "UNCHECKED" would be indistinguishable from an honest one.
func (s MandateStatus) String() string {
	switch s {
	case MandateUnchecked:
		return "UNCHECKED"
	case MandateFeasible:
		return "FEASIBLE"
	case MandateInfeasible:
		return "INFEASIBLE"
	}
	return "MandateStatus(" + strconv.Itoa(int(s)) + ")"
}

// MarshalJSON renders the verdict as its NAME.
//
// A bare int on the wire would put the unchecked state on the wire as 0, which
// reads as absent, false and "fine" in every JSON client there is — the same
// collapse the bool had. An unknown value is an error rather than a number,
// because a proposal whose verdict cannot be named must not leave this process.
func (s MandateStatus) MarshalJSON() ([]byte, error) {
	switch s {
	case MandateUnchecked, MandateFeasible, MandateInfeasible:
		return json.Marshal(s.String())
	}
	return nil, fmt.Errorf("optimization: refusing to serialize an unknown MandateStatus %d", int(s))
}

// UnmarshalJSON accepts ONLY the three names, case-insensitively, and an absent
// or empty value as UNCHECKED.
//
// It exists so that a body carrying a verdict is DECODED IN ORDER TO BE REFUSED
// (the treatment ordersRequest.Issuer already gets), never quietly reinterpreted.
// A pre-#646 client sending the boolean true gets an error naming the three
// legal values, not a silent MandateFeasible.
func (s *MandateStatus) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("optimization: a mandate verdict is one of UNCHECKED, FEASIBLE or "+
			"INFEASIBLE, not %s", string(b))
	}
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "", "UNCHECKED":
		*s = MandateUnchecked
	case "FEASIBLE":
		*s = MandateFeasible
	case "INFEASIBLE":
		*s = MandateInfeasible
	default:
		return fmt.Errorf("optimization: unknown mandate verdict %q — it is one of UNCHECKED, "+
			"FEASIBLE or INFEASIBLE", name)
	}
	return nil
}

// RebalanceProposal is the optimizer's output — target weights + the minimal
// trade list to reach them, plus the ex-ante stats and the mandate verdict.
// The Go-native shape of optimization.v1.RebalanceProposal.
type RebalanceProposal struct {
	PortfolioID    string
	Objective      Objective
	Targets        map[string]float64
	Trades         []ProposedTrade
	ExpectedReturn float64
	// ExpectedRisk is the ex-ante √(wᵀΣw) of the target book, and is NIL when the
	// covariance could not support one — see Result.ExpectedRisk for why it is a
	// pointer. On the wire that is `"ExpectedRisk": null`, which no client reads
	// as a riskless portfolio; the 0 it used to send does (#621).
	ExpectedRisk *float64
	// CovarianceQuality is WHAT WAS ESTABLISHED ABOUT Σ before the numbers above
	// were computed. Like MandateStatus it is UNCHECKED by construction: Rebalance
	// is handed no covariance, so a proposal it builds says nothing about one.
	CovarianceQuality CovarianceQuality
	Turnover          float64
	AsOf              time.Time
	// MandateStatus is WHETHER A MANDATE WAS CONSULTED AND WHAT IT SAID. Only
	// CheckMandate, through Propose, may set it to anything but MandateUnchecked;
	// a value arriving from outside this package's own pipeline is a claim its
	// bearer is not entitled to make, and the optimization service's HTTP surface
	// refuses a request body that carries one.
	MandateStatus MandateStatus
	Violations    []string
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
	// NO MandateStatus HERE, AND THAT IS THE POINT (#646). Rebalance is handed no
	// mandate, no engine and no classifier, so it is in no position to say
	// anything about compliance; the zero value says exactly that. It used to
	// stamp MandateFeasible: true, which is how every proposal this platform ever
	// built came out certified by a check that had not run.
	//
	// NO CovarianceQuality AND NO ExpectedRisk EITHER, for the same reason: it is
	// handed no covariance. Both stay at their zero values — UNCHECKED and nil —
	// and only Propose, which ran Optimize, overwrites them (#621).
	return RebalanceProposal{
		PortfolioID: portfolioID,
		Targets:     target,
		Trades:      trades,
		Turnover:    gross / 2,
		AsOf:        asOf,
	}
}

// Propose runs the full OPT-01 pipeline: optimize the targets, check them
// against the COMP-01 mandate (deny-by-default), and diff into a minimal trade
// list — the one call a PM workflow / the OPT-01e service drives. The mandate
// constraints are folded into the optimization box up front (a book you can't
// hold, you can't optimize into) AND the result is re-checked against the full
// mandate; an infeasible result is still returned (MandateInfeasible +
// violations) so the PM sees why, never silently dropped.
//
// A NIL MANDATE DOES NOT PASS. CheckMandate reports MandateUnchecked for it, and
// the proposal carries that verdict out — so "this deployment has no mandate for
// the portfolio" and "the mandate was satisfied" are different answers, and only
// the second materializes into orders.
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
	proposal.CovarianceQuality = res.CovarianceQuality

	status, violations := CheckMandate(ctx, res.Weights, nav, currency, classifier, engine, mandate, asOf)
	proposal.MandateStatus = status
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
