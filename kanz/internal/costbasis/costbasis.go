// Package costbasis is the platform's ONE weighted-average-cost fold (#428).
//
// It answers a single question: given an open lot and a signed fill at a price,
// what is the new quantity, the new average cost, and how much P&L was realized?
// Every place on this platform that claims to know a position's basis — the OMS's
// in-memory book, its durable Postgres book, the accounting IBOR, the TradingView
// projection, and the backtest that #428 builds — is answering that question, and
// until now each answered it with its own copy of the arithmetic.
//
// THREE COPIES EXISTED, AND THEY HAD ALREADY DRIFTED. That is not a hypothetical
// risk; it is the state this package was extracted from:
//
//   - services/oms/internal/position (book.go and postgres.go share foldLot)
//   - services/accounting/internal/ledger (foldPosition), whose own comment named
//     the problem: "this function is a hand-copy of the OMS fold … Two copies of
//     one calculation is the standing risk; the next edit to either should not
//     have to rediscover which one had the guard."
//   - services/tv-sync/internal/projection (applyExecution)
//
// The drift was real and visible in both directions:
//
//  1. THE ZERO-DIVISOR GUARD. big.Rat.Quo PANICS on a zero divisor, and the
//     divisor here is derived rather than validated. #217 crash-looped the estate
//     on exactly that line. The guard was then added to the OMS fold and
//     hand-copied to accounting's — leaving each copy to be discovered separately,
//     which is precisely what the accounting comment warned about.
//  2. FEES. tv-sync subtracted the fill's fee from realized P&L; the OMS and
//     accounting folds did not. So "what has this position realized" had two
//     different answers depending on which surface a person was looking at.
//
// FEE POLICY IS DELIBERATELY NOT IN HERE. Fold computes GROSS realized P&L —
// (price − avg) × closed — because that is the arithmetic all four consumers
// share. Whether fees reduce a reported realized figure is a REPORTING decision
// that differs legitimately by surface, and burying it in the shared fold is how
// it became invisible in the first place. A caller that nets fees does so on its
// own line, where a reader can see it.
package costbasis

import "math/big"

// Lot is the running state of one holding.
//
// Qty is SIGNED (positive long, negative short); AvgCost is the average cost of
// the currently-open position and is always non-negative; Realized accumulates
// gross realized P&L.
//
// AN INVARIANT WORTH KNOWING: whenever Qty is zero, AvgCost is zero. A holding
// that nets to nothing has no basis, and reporting a stale average against a flat
// position would give an unrealized figure for something nobody holds.
type Lot struct {
	Qty      *big.Rat
	AvgCost  *big.Rat
	Realized *big.Rat
}

// NewLot returns a flat lot. The fields are never nil, because every operation
// here mutates them in place and a nil *big.Rat is a panic one dereference later.
func NewLot() *Lot {
	return &Lot{Qty: new(big.Rat), AvgCost: new(big.Rat), Realized: new(big.Rat)}
}

// Fold applies a signed fill quantity at price, mutating l.
//
// The three cases, and why each is what it is:
//
//   - FLAT OR SAME DIRECTION — the position grows and the cost re-averages:
//     avg = (|old|·avg + |add|·price) / |old + add|. No P&L is realized, because
//     nothing was closed.
//   - OPPOSITE DIRECTION — P&L is realized on the closed portion only, against the
//     average rather than against any particular earlier fill. Closing more than
//     is open realizes only the open portion; the excess opens a new position.
//   - CROSSING THROUGH ZERO — the new lot opens at THIS fill's price. Carrying the
//     old average across a flip would leave a long's basis attached to a short.
//
// A ZERO QUANTITY IS A NO-OP, NOT A PANIC. Both OMS callers screen a non-positive
// fill before reaching here and accounting screens a zero, so the guard below is
// unreachable through any current caller — and it stays anyway, for the reason
// #217 taught: the divisor is derived here rather than validated here, the failure
// mode is taking the process down rather than returning a wrong number, and the
// whole point of one shared fold is that the next caller does not have to
// rediscover which copy was guarded.
func Fold(l *Lot, signed, price *big.Rat) {
	cur := l.Qty.Sign()
	add := signed.Sign()

	if cur == 0 || cur == add {
		oldAbs := new(big.Rat).Abs(l.Qty)
		addAbs := new(big.Rat).Abs(signed)
		newQty := new(big.Rat).Add(l.Qty, signed)
		newAbs := new(big.Rat).Abs(newQty)
		// Reachable only when the lot AND the incoming quantity are both zero.
		// Flat is a real state, not an error.
		if newAbs.Sign() == 0 {
			l.AvgCost = new(big.Rat)
			l.Qty = newQty
			return
		}
		cost := new(big.Rat).Mul(oldAbs, l.AvgCost)
		cost.Add(cost, new(big.Rat).Mul(addAbs, price))
		l.AvgCost = new(big.Rat).Quo(cost, newAbs)
		l.Qty = newQty
		return
	}

	closeAbs := new(big.Rat).Abs(signed)
	openAbs := new(big.Rat).Abs(l.Qty)
	if closeAbs.Cmp(openAbs) > 0 {
		closeAbs = openAbs // crossing through zero: only the open portion closes
	}
	// realized += closed × (price − avg), negated for a short, which profits when
	// the price falls below its average.
	pnl := new(big.Rat).Mul(closeAbs, new(big.Rat).Sub(price, l.AvgCost))
	if cur < 0 {
		pnl.Neg(pnl)
	}
	l.Realized.Add(l.Realized, pnl)

	newQty := new(big.Rat).Add(l.Qty, signed)
	switch {
	case newQty.Sign() == 0:
		l.Qty = new(big.Rat)
		l.AvgCost = new(big.Rat)
	default:
		if newQty.Sign() != cur {
			l.AvgCost = new(big.Rat).Set(price)
		}
		l.Qty = newQty
	}
}

// Aggregator sums several lots of the SAME instrument — one per venue — into the
// fund-level holding.
//
// A SUM OF QUANTITIES, BUT A COST-WEIGHTED AVERAGE OF BASES. Quantities and
// realized P&L add; average cost does not, and adding it (or taking the mean of
// the venue averages) would report a basis nobody paid whenever the venues hold
// unequal sizes. The weight is |quantity|, so a venue holding ten units moves the
// fund's basis ten times as much as one holding a single unit.
//
// THREE COPIES OF THIS EXISTED TOO, all inside the OMS's position package: the
// in-memory book's aggregate, the durable store's, and the snapshot's — differing
// only in whether the lots came from a map or from SQL rows. It lives here for the
// same reason Fold does.
//
// ABSOLUTE VALUES ARE WHAT MAKE A HEDGED FUND'S BASIS SURVIVE. A long of 10 and a
// short of 10 net to zero quantity; weighting by the SIGNED quantity would divide
// by zero, and weighting by it without the guard below would panic. Weighting by
// the absolute quantity keeps a meaningful average across both legs.
type Aggregator struct {
	agg    *Lot
	cost   *big.Rat
	absQty *big.Rat
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{agg: NewLot(), cost: new(big.Rat), absQty: new(big.Rat)}
}

// Add folds one venue's lot into the aggregate.
func (a *Aggregator) Add(l *Lot) {
	a.agg.Qty.Add(a.agg.Qty, l.Qty)
	a.agg.Realized.Add(a.agg.Realized, l.Realized)
	abs := new(big.Rat).Abs(l.Qty)
	a.absQty.Add(a.absQty, abs)
	a.cost.Add(a.cost, new(big.Rat).Mul(abs, l.AvgCost))
}

// Lot returns the aggregated holding.
//
// A ZERO TOTAL LEAVES THE AVERAGE AT ZERO rather than dividing — the same reason
// Fold guards its divisor, and reachable here in an ordinary way rather than a
// contrived one: a portfolio that has closed every venue, or one perfectly hedged
// across two. Flat is a real state with no basis.
func (a *Aggregator) Lot() *Lot {
	if a.absQty.Sign() != 0 {
		a.agg.AvgCost = new(big.Rat).Quo(a.cost, a.absQty)
	}
	return a.agg
}

// Unrealized returns Qty × (mark − AvgCost), or nil when the lot is flat or there
// is no mark.
//
// NIL IS NOT ZERO, and the difference is the point: an unmarked position has an
// unknown unrealized P&L, while a flat one genuinely has none. A caller that
// rendered both as 0 would report a held position as breaking even.
func (l *Lot) Unrealized(mark *big.Rat) *big.Rat {
	if mark == nil || l.Qty.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Mul(new(big.Rat).Sub(mark, l.AvgCost), l.Qty)
}
