// Package accounting is the IBOR's valuation layer (IBOR-01d): daily NAV over the
// folded book, the P&L attribution of a NAV change to its drivers (price / cash /
// fx / corporate-action), and the interest/dividend accruals that carry
// earned-not-received income in NAV. It sits on top of the event-sourced ledger:
// the book is the state, this package values it.
package accounting

import (
	"fmt"
	"math/big"
	"time"

	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

// NAV is a point-in-time valuation of a book: the headline total and its cash /
// security / accrued components, with the attribution of the change since a prior
// valuation. total = cash + securityValue + accrued.
type NAV struct {
	PortfolioID   string
	Currency      string
	Total         *big.Rat
	Cash          *big.Rat
	SecurityValue *big.Rat
	Accrued       *big.Rat
	AsOf          time.Time
	Attribution   []PnlComponent
}

// PnlComponent is one attributed driver of a NAV change. The components sum to
// the total change.
type PnlComponent struct {
	Source string // "price", "cash", "fx", "corporate_action"
	Amount *big.Rat
}

// ComputeNAV values a book in its reporting currency at the given prices. Every
// non-flat position must have a price — an incomplete valuation never produces a
// NAV (the same completeness discipline as a regulatory filing). Positions and
// cash are assumed in the reporting currency (the single-currency book; FX is the
// attribution axis below and a carried-forward multi-currency seam).
func ComputeNAV(b *ledger.Book, currency string, asOf time.Time, prices map[string]*big.Rat) (NAV, error) {
	sec := new(big.Rat)
	for inst, p := range b.Positions {
		if p.Qty.Sign() == 0 {
			continue
		}
		px, ok := prices[inst]
		if !ok {
			return NAV{}, fmt.Errorf("accounting: NAV missing price for %q", inst)
		}
		sec.Add(sec, new(big.Rat).Mul(p.Qty, px))
	}
	cash := b.CashBalance(currency)
	accrued := b.AccruedBalance(currency)
	total := new(big.Rat).Add(cash, sec)
	total.Add(total, accrued)
	return NAV{
		PortfolioID:   b.PortfolioID,
		Currency:      currency,
		Total:         total,
		Cash:          cash,
		SecurityValue: sec,
		Accrued:       accrued,
		AsOf:          asOf,
	}, nil
}

// Attribute decomposes the NAV change from prior to current into its drivers and
// returns current with Attribution set. The period entries are those folded
// between the two valuations (used to separate trade cash from external flows);
// fx is the currency-revaluation effect (zero for a single-currency book).
//
// Identity: price + cash + fx + corporate_action == current.Total − prior.Total,
// where
//   - cash             = external cash flows (subscriptions/redemptions/fees),
//   - corporate_action = dividend/coupon/merger cash + the change in accrued income,
//   - price            = mark-to-market = Δ security value + trade cash − fx,
//   - fx               = the supplied currency revaluation.
func Attribute(prior, current NAV, periodEntries []*ledger.Event, fx *big.Rat) NAV {
	if fx == nil {
		fx = new(big.Rat)
	}
	tradeCash := new(big.Rat)
	external := new(big.Rat)
	for _, e := range periodEntries {
		if e.Cash == nil {
			continue
		}
		switch e.Type {
		case ledger.EntryTrade:
			tradeCash.Add(tradeCash, e.Cash)
		case ledger.EntryCash, ledger.EntryFee:
			external.Add(external, e.Cash)
		}
	}

	deltaSec := new(big.Rat).Sub(current.SecurityValue, prior.SecurityValue)
	deltaAccrued := new(big.Rat).Sub(current.Accrued, prior.Accrued)

	// price = Δ security value + trade cash − fx (the mark-to-market: holdings
	// revalued, plus price moves since each in-period trade).
	price := new(big.Rat).Add(deltaSec, tradeCash)
	price.Sub(price, fx)

	// corporate-action income = total cash change not explained by trades or
	// external flows, plus accrued-income earned in the period.
	deltaCash := new(big.Rat).Sub(current.Cash, prior.Cash)
	corpCash := new(big.Rat).Sub(deltaCash, tradeCash)
	corpCash.Sub(corpCash, external)
	corpAction := new(big.Rat).Add(corpCash, deltaAccrued)

	current.Attribution = []PnlComponent{
		{Source: "price", Amount: price},
		{Source: "cash", Amount: external},
		{Source: "fx", Amount: new(big.Rat).Set(fx)},
		{Source: "corporate_action", Amount: corpAction},
	}
	return current
}

// AttributionTotal sums the attribution components — equals current.Total −
// prior.Total by the identity above.
func AttributionTotal(n NAV) *big.Rat {
	sum := new(big.Rat)
	for _, c := range n.Attribution {
		sum.Add(sum, c.Amount)
	}
	return sum
}

// StraightLineAccrual is the linearly-accreted portion of a known income amount
// (a bond coupon, a declared dividend) earned between start and end as of asOf —
// amount × elapsed/total, clamped to [0, amount]. It is the actual/actual day-
// count accrual the book posts daily so NAV reflects earned-not-received income.
func StraightLineAccrual(amount *big.Rat, start, end, asOf time.Time) *big.Rat {
	if amount == nil || !end.After(start) {
		return new(big.Rat)
	}
	if !asOf.After(start) {
		return new(big.Rat)
	}
	if !asOf.Before(end) {
		return new(big.Rat).Set(amount)
	}
	elapsed := big.NewRat(int64(asOf.Sub(start)), 1)
	totalDur := big.NewRat(int64(end.Sub(start)), 1)
	frac := new(big.Rat).Quo(elapsed, totalDur)
	return new(big.Rat).Mul(amount, frac)
}

// AccrualEntry builds the journal posting that books accrued income to asOf —
// an EntryAccrual carrying the incremental accrual since the prior posting. The
// caller passes the increment (accrued-now minus accrued-already-posted) so the
// book's accrued balance tracks the schedule.
func AccrualEntry(entryID, portfolioID, instrument, currency string, increment *big.Rat, asOf, knowledge time.Time) *ledger.Event {
	return &ledger.Event{
		EntryID:      entryID,
		PortfolioID:  portfolioID,
		Type:         ledger.EntryAccrual,
		InstrumentID: instrument,
		Cash:         increment,
		CashCurrency: currency,
		Effective:    asOf,
		Knowledge:    knowledge,
		SourceRef:    instrument,
	}
}
