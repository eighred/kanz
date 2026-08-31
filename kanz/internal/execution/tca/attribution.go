package tca

import (
	"fmt"
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// IMPLEMENTATION SHORTFALL, DECOMPOSED (#866).
//
// Measure above answers "what did this fill cost" against the decision-time
// mark. This answers the question a desk actually acts on: WHY. A parent that
// cost 14bps against arrival tells nobody whether to change algorithm, change
// venue or change urgency — those are three different repairs and a single
// number chooses none of them.
//
// # The grain is the DECISION
//
// One attribution per parent, never per slice. A slice is not a decision; it is
// a consequence of one, and #435's own admission comment states the reason in
// the other direction: a child inherits the parent's arrival mark precisely so
// that working an order on a schedule cannot report itself as costless. The same
// logic forbids attributing to a child at all.
//
// # Two legs are measured and one is a residual, stated plainly
//
// Market impact is not observable. Nobody can see the price this order would
// have filled at had it never been sent, so any claim to have measured impact
// directly is a model wearing a measurement's clothes. Spread and timing ARE
// observable, each as a difference between two observations the OMS recorded at
// defined instants:
//
//	spread  = the half-width of the market QUOTED when each slice was released
//	timing  = how far the mid moved between the DECISION and each slice's release
//	impact  = shortfall - spread - timing
//
// The identity is exact by construction, which is why the sum test in this
// package proves something rather than restating an assignment: the total comes
// from the fills, the spread from the quotes, the timing from two marks, and
// only the residual is defined by the equation. A three-way split derived from
// one number would be arithmetic dressing, and it is what this deliberately is
// not.
//
// # UNKNOWN IS NOT ZERO, AND HERE IT IS THE WHOLE DECOMPOSITION
//
// A slice released into an instrument the spine never quoted has no observable
// width. Scoring its spread as zero would claim the fund crossed for free and
// would push the entire cost into impact, which is the one leg nobody can check.
// So a decomposition is ALL-OR-NOTHING per decision: one unmeasurable slice and
// the three legs come back nil with QualityTotalOnly. The headline shortfall is
// still exact and still published — it only needs arrival and the fills — which
// is the honest two-part answer rather than a fabricated three-part one.

// Quality says how much of a decomposition was observed rather than inferred.
type Quality int

const (
	// QualityUnknown is the zero value and is never returned by Attribute. It
	// exists so a Attribution that was never computed cannot be read as one that
	// was and came back empty.
	QualityUnknown Quality = iota
	// QualityDecomposed: every slice that traded carried a release mark and a
	// quoted touch, so all three legs are present and sum to the shortfall.
	QualityDecomposed
	// QualityTotalOnly: the shortfall is exact and the legs are nil, because at
	// least one slice that traded could not be observed.
	QualityTotalOnly
)

func (q Quality) String() string {
	switch q {
	case QualityDecomposed:
		return "DECOMPOSED"
	case QualityTotalOnly:
		return "TOTAL_ONLY"
	default:
		return "UNKNOWN"
	}
}

// Slice is one order that traded into a decision: a child of a worked parent, or
// the decision's own order when it was not sliced.
//
// FilledQuantity and AveragePrice are what that order actually did. The three
// Release fields are the market as it stood when that order was admitted —
// OrderState.release_price, release_bid and release_ask — and any of them being
// nil makes this slice unmeasurable, which makes the whole decision's
// decomposition unmeasurable. See the package comment for why that is
// all-or-nothing rather than per-slice.
type Slice struct {
	OrderID        string
	FilledQuantity *big.Rat
	AveragePrice   *big.Rat
	ReleaseMark    *big.Rat
	ReleaseBid     *big.Rat
	ReleaseAsk     *big.Rat
}

// measurable reports whether this slice carries everything the decomposition
// needs. A CROSSED TOUCH IS NOT MEASURABLE: ask <= bid is a book this platform
// cannot have seen in one consistent state, and a negative half-spread would
// arrive in the report as the fund being PAID to take liquidity.
func (s Slice) measurable() bool {
	return s.ReleaseMark != nil && s.ReleaseMark.Sign() > 0 &&
		s.ReleaseBid != nil && s.ReleaseAsk != nil &&
		s.ReleaseBid.Sign() > 0 && s.ReleaseAsk.Cmp(s.ReleaseBid) > 0
}

// Attribution is one decision's realized cost against arrival, decomposed.
//
// EVERY FIGURE IS SIGNED FROM THE FUND'S POINT OF VIEW — positive cost money,
// negative beat the benchmark — the same convention Result uses, chosen once so
// a report cannot average a good sell against a bad buy to zero.
//
// EVERY FIGURE EXCLUDES FEES. A fee is charged per fill and the individual
// Fill's fee exists only on the fill FACT, never on the stored OrderState this
// is built from; Measure/TransactionCostRecorded is the fee-inclusive, per-fill
// measure and stays the place to ask that question.
type Attribution struct {
	Side           orderpb.Side
	FilledQuantity *big.Rat
	// NotionalTraded is Σ(q × p) over the slices, and AveragePrice is that
	// divided by the quantity: the decision's quantity-weighted achieved price.
	NotionalTraded *big.Rat
	AveragePrice   *big.Rat
	ArrivalPrice   *big.Rat

	// ShortfallBps is the headline, in basis points of ARRIVAL notional. Always
	// present — it needs only the arrival mark and the fills.
	ShortfallBps *big.Rat

	// The legs. All three nil together when Quality is QualityTotalOnly; nil is
	// the value that says "not observed", and it is not zero.
	SpreadBps *big.Rat
	ImpactBps *big.Rat
	TimingBps *big.Rat

	Quality Quality
	// Slices counts the orders that traded into this decision; Measured counts
	// how many of those carried the observations the legs need.
	//
	// BOTH ARE CARRIED EVEN THOUGH Quality ALREADY SAYS DECOMPOSED OR NOT,
	// because "one slice of fifty lost its quote" and "none of them ever had one"
	// are the same Quality and two entirely different operational problems.
	Slices   int
	Measured int
}

// Attribute decomposes one decision's implementation shortfall against arrival.
//
// arrival is the mark stamped on the DECISION at admission — never a price
// looked up now. A mark read after the fact is a mark from after this order
// moved the market, and it biases every result flatter, always in the flattering
// direction.
//
// Slices with no quantity are ignored rather than refused: a child that was
// admitted and cancelled before it traded is a normal outcome of working an
// order, and it contributes nothing to a cost the fund did not pay. It is not
// counted as an unmeasurable slice either — there is nothing about it to
// measure, so letting it degrade the whole decision to TOTAL_ONLY would report
// a price-spine gap that does not exist.
func Attribute(side orderpb.Side, arrival *big.Rat, slices []Slice) (Attribution, error) {
	if arrival == nil || arrival.Sign() <= 0 {
		return Attribution{}, ErrNoArrivalMark
	}

	qty, notional := new(big.Rat), new(big.Rat)
	// spreadCost is Σ q×(ask-bid)/2 and driftCost is Σ q×sign×(releaseMark-arrival),
	// both in currency. They become basis points once, at the end, against the
	// same arrival notional the headline uses — so the three figures are on one
	// scale and the identity below is exact rather than approximately true.
	spreadCost, driftCost := new(big.Rat), new(big.Rat)
	traded, measured := 0, 0
	allMeasurable := true

	sign := big.NewRat(1, 1)
	if side == orderpb.Side_SIDE_SELL {
		sign = big.NewRat(-1, 1)
	}
	half := big.NewRat(1, 2)

	for _, sl := range slices {
		q := sl.FilledQuantity
		if q == nil || q.Sign() <= 0 {
			continue
		}
		if sl.AveragePrice == nil || sl.AveragePrice.Sign() <= 0 {
			// A slice that traded at a price nobody recorded cannot be averaged
			// in, and dropping it silently would understate the decision's size
			// while the shortfall it produced stayed in nothing.
			return Attribution{}, fmt.Errorf(
				"tca: slice %s filled %s at no recorded price, so this decision cannot be measured",
				sl.OrderID, q.FloatString(12))
		}
		traded++
		qty.Add(qty, q)
		notional.Add(notional, new(big.Rat).Mul(q, sl.AveragePrice))

		if !sl.measurable() {
			allMeasurable = false
			continue
		}
		measured++
		width := new(big.Rat).Sub(sl.ReleaseAsk, sl.ReleaseBid)
		spreadCost.Add(spreadCost, new(big.Rat).Mul(q, new(big.Rat).Mul(width, half)))
		drift := new(big.Rat).Sub(sl.ReleaseMark, arrival)
		drift.Mul(drift, sign)
		driftCost.Add(driftCost, new(big.Rat).Mul(q, drift))
	}
	if qty.Sign() == 0 {
		return Attribution{}, ErrNoFills
	}

	avg := new(big.Rat).Quo(notional, qty)
	// THE ARRIVAL NOTIONAL IS THE DENOMINATOR FOR ALL FOUR FIGURES — the
	// benchmark's own scale, not the achieved one. Dividing by what was actually
	// paid would make a worse execution look proportionally smaller, and would
	// put the legs on a different scale from the total they must sum to.
	arrivalNotional := new(big.Rat).Mul(qty, arrival)
	tenThousand := big.NewRat(10000, 1)
	bps := func(cost *big.Rat) *big.Rat {
		r := new(big.Rat).Quo(cost, arrivalNotional)
		return r.Mul(r, tenThousand)
	}

	priceCost := new(big.Rat).Mul(new(big.Rat).Sub(avg, arrival), sign)
	priceCost.Mul(priceCost, qty)
	total := bps(priceCost)

	a := Attribution{
		Side:           side,
		FilledQuantity: qty,
		NotionalTraded: notional,
		AveragePrice:   avg,
		ArrivalPrice:   new(big.Rat).Set(arrival),
		ShortfallBps:   total,
		Quality:        QualityTotalOnly,
		Slices:         traded,
		Measured:       measured,
	}
	if !allMeasurable {
		// THE LEGS STAY NIL. Returning zeros here is the single most damaging
		// thing this function could do: it would claim a free crossing and no
		// drift, and push every unexplained basis point into impact — the one
		// leg nobody can independently check.
		return a, nil
	}
	spread := bps(spreadCost)
	timing := bps(driftCost)
	// THE RESIDUAL, AND IT IS THE ONLY FIGURE HERE DEFINED BY THE EQUATION
	// RATHER THAN BY AN OBSERVATION. Everything it absorbs is real cost that the
	// quoted market and the market's own drift do not account for.
	impact := new(big.Rat).Sub(total, spread)
	impact.Sub(impact, timing)

	a.SpreadBps, a.TimingBps, a.ImpactBps = spread, timing, impact
	a.Quality = QualityDecomposed
	return a, nil
}
