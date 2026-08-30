package compliance

import (
	"fmt"
	"math/big"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	// decutil: this package's own tests declare a local `dec(...)` Decimal-literal
	// helper (engine_test.go), so the platform decimal package is aliased here for
	// the reason gate.go states — a name clash, not a preference.
	decutil "github.com/eighred/kanz/internal/dec"
)

// MarkSource supplies a reference price for an instrument, or nil when there is
// none it may act on — never a zero, never a guess. It is satisfied by
// internal/marketdata/mark.Source.
//
// THE SAME SHAPE THE OMS ALREADY DEFINED for pricing a MARKET order
// (services/oms/internal/compliance.MarkSource). It is restated here rather than
// imported because this package is BELOW that one and cannot import it; the two
// are structurally identical, so one value satisfies both, and the arch guard on
// duplicated helpers is about copied LOGIC rather than a one-method seam each
// layer names for itself.
type MarkSource interface {
	Mark(instrument string) *big.Rat
}

// CashSource answers what a portfolio can spend, or ok=false when it is UNKNOWN.
// It is satisfied by internal/cashview.View, which folds the book of record's
// announcements.
//
// A SEAM AND NOT A QUERY. Both callers read this while deciding something — the
// OMS on the order-admission path, the monitor inside a FACT delivery — and a
// call into accounting from either would put an externally-owned latency in
// front of the decision and turn a degraded accounting service into a trading
// outage.
//
// completeness is what the ANNOUNCING deployment said that total contains
// (#614), or nil when it said nothing — which is not the same as "nothing is
// missing" and must not be flattened into it.
type CashSource interface {
	Spendable(portfolioID string) (total *commonpb.Decimal, currency string, completeness *CashCompleteness, ok bool)
}

// JoinEquity attaches a portfolio's cash to a book and then establishes its
// equity. It is the whole "what is this book worth" sequence, in one place.
//
// ORDER IS LOAD-BEARING, WHICH IS WHY THIS IS A FUNCTION AND NOT A CONVENTION.
// Equity is marked positions PLUS cash, so running MarkEquity before the cash
// join computes a positions total and then asserts it is equity — which is
// exactly the defect #780 was about, made one line earlier. Two callers each
// writing the sequence themselves is two chances to get that order wrong, and
// the wrong one produces a plausible number rather than an error.
//
// A nil cash source leaves Cash UNKNOWN, which MarkEquity refuses to guess past:
// unknown cash and an empty account are different books and only one of them is
// levered. A nil mark source leaves the book on its producer's declared basis.
//
// It never fails. A book it cannot value comes back exactly as it went in, with
// the reason on NAVBasisDetail; every caller of this is on a path where refusing
// to RETURN a book would be worse than returning one whose leverage rule refuses.
func JoinEquity(b *Book, cash CashSource, marks MarkSource) {
	if b == nil {
		return
	}
	if cash != nil {
		if total, currency, completeness, ok := cash.Spendable(b.PortfolioID); ok {
			b.Cash = &commonpb.Money{Amount: total, CurrencyCode: currency}
			b.CashCompleteness = completeness
		}
	}
	MarkEquity(b, marks)
}

// MarkEquity turns the OMS's cost-basis book into a MARKED book with a real
// equity figure — or changes nothing at all and records why (#780).
//
// # What it replaces
//
// The position book folds fills, so it knows quantity and average cost and
// nothing else: its Snapshot values every holding at what it was BOUGHT for and
// reports the sum of those as TotalMarketValue, which BookFromSnapshot hands to
// the gate as NAV. Two things were wrong with using that as a leverage
// denominator, and the first is fatal on its own:
//
//   - It excludes cash. Gross exposure is the sum of the ABSOLUTE position
//     values; NAV was the sum of the same values. For a book with no shorts the
//     ratio is 1.0 BY CONSTRUCTION — a max_gross_leverage of 1.5 could not bind
//     however the fund was financed, and a portfolio 95% in cash scored the same
//     as one fully invested.
//   - It is at cost. A ratio whose numerator is what the book cost and whose
//     denominator is what it is worth is neither number.
//
// # All or nothing, deliberately
//
// Equity is established only when EVERY holding has a live mark, cash is known,
// and all three currencies agree. Anything less and this function returns the
// book exactly as it found it, on its declared NAVBasisGrossPositions.
//
// IT DOES NOT PARTIALLY UPGRADE THE BOOK, and that is the important property.
// Marking the holdings it can and leaving the rest at cost would produce a
// numerator that is a mixture of two bases, and a denominator that is a mixture
// of the same two — a ratio that is precise, plausible, and means nothing.
// Marking some positions and blanking the others would be worse still: an
// absent MarketValue makes unmarkedHoldings refuse EVERY rule that reads the
// book (#760), so a single stalled instrument would stop concentration and
// restriction limits that are correct today from evaluating at all — a trading
// outage delivered by a leverage repair.
//
// So the failure mode is bounded to the one rule that needs equity:
// LeverageRule refuses, naming the reason this function recorded, and every
// other rule sees the book it saw before.
func MarkEquity(b *Book, marks MarkSource) {
	if marks == nil {
		// Nothing tried, so nothing to explain: the deployment has no price feed
		// wired, which the gate already says at startup. A detail here would read
		// as a fault where the truth is a posture.
		return
	}
	equity, marked, err := equityFromMarks(b, marks)
	if err != nil {
		b.NAVBasisDetail = err.Error()
		return
	}
	amount, ok := decutil.ToProtoScaled(equity)
	if !ok {
		b.NAVBasisDetail = "equity is not representable as a Decimal"
		return
	}
	// COMMITTED IN ONE STEP, after every position has been valued. Writing the
	// marks in as they are computed would leave a half-marked book behind on the
	// first instrument that has no price — the mixed basis this function exists
	// to avoid.
	for i := range b.Positions {
		b.Positions[i].MarketValue = marked[i]
	}
	b.NAV = &commonpb.Money{Amount: amount, CurrencyCode: b.BaseCurrency}
	b.NAVBasis = NAVBasisEquity
	b.NAVBasisDetail = ""
}

// equityFromMarks values every holding at its live mark and adds cash, or
// refuses with the reason. It reads the book and does not mutate it, so a
// refusal cannot leave a partially rewritten book behind.
//
// THE MARK IS ASSUMED TO BE IN THE PORTFOLIO'S BASE CURRENCY, which is the same
// assumption COMP01Gate.OrderPrice already makes when it values a MARKET order
// (the OMS has no per-instrument currency join, OMS-01e). It is stated rather
// than hidden: a book whose positions are NOT already denominated in the base
// currency is refused below rather than summed, because that is the case where
// the assumption is demonstrably false and the sum would be economically
// meaningless — the netting error decutil.MoneyIn exists to stop.
func equityFromMarks(b *Book, marks MarkSource) (*big.Rat, []*commonpb.Money, error) {
	if b.BaseCurrency == "" {
		return nil, nil, fmt.Errorf("portfolio has no base currency, so nothing can be summed into equity")
	}
	// CASH IS NOT OPTIONAL HERE, and its absence is the ordinary case rather than
	// a fault: cash comes from the book of record and is UNKNOWN for a portfolio
	// accounting has never announced. Unknown cash cannot be treated as zero — an
	// empty account and an unmeasured one are different books, and only one of
	// them is levered.
	if b.Cash == nil {
		return nil, nil, fmt.Errorf("cash is unknown, and equity without cash is a positions total")
	}
	equity, err := decutil.MoneyIn(b.Cash, b.BaseCurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("cash cannot be netted into equity: %w", err)
	}
	equity = new(big.Rat).Set(equity)

	marked := make([]*commonpb.Money, len(b.Positions))
	for i, p := range b.Positions {
		if p.Quantity == nil {
			return nil, nil, fmt.Errorf("position %s carries no quantity, so it cannot be valued", p.InstrumentID)
		}
		// A MARK CARRYING A NUMBER AND NO UNIT CANNOT BE SHOWN TO BE IN THE BASE
		// CURRENCY (#806), and the check below cannot see it: `!= ""` SKIPS it, so
		// a holding whose currency nobody stated was silently valued at the
		// base-currency mark and netted into equity. Equity is LeverageRule's
		// denominator, so the fail-open direction is a book that reads as less
		// levered for carrying a position nobody stated the currency of.
		//
		// This is the same absence rules.go's unmarkedReason refuses one function
		// over; it is spelled separately because the two are asking different
		// questions — that one asks whether the mark can be USED, this one asks
		// whether the holding is FOREIGN — and an unstated currency is unknown to
		// both.
		if mv := p.MarketValue; mv != nil && mv.GetAmount() != nil && mv.GetCurrencyCode() == "" {
			return nil, nil, fmt.Errorf("position %s carries a market value with no currency code, "+
				"so it cannot be shown to be in the portfolio's %s and must not be summed into equity",
				p.InstrumentID, b.BaseCurrency)
		}
		// A POSITION ALREADY IN ANOTHER CURRENCY FALSIFIES THE ASSUMPTION ABOVE.
		// A nil MarketValue, or one with no amount, is not that — it is a holding
		// the book has not priced, which is exactly what a mark is for.
		if mv := p.MarketValue; mv != nil && mv.GetCurrencyCode() != "" && mv.GetCurrencyCode() != b.BaseCurrency {
			return nil, nil, fmt.Errorf("position %s is held in %s, not the portfolio's %s",
				p.InstrumentID, mv.GetCurrencyCode(), b.BaseCurrency)
		}
		px := marks.Mark(p.InstrumentID)
		if px == nil {
			// mark.Source returns nil for never-seen AND for expired, and refuses to
			// distinguish them on a decision path on purpose — its Lookup accessor is
			// the diagnostic one and nothing here may call it. Either way there is no
			// price this may act on.
			return nil, nil, fmt.Errorf("no live mark for %s", p.InstrumentID)
		}
		qty, ok := decutil.FromProtoChecked(p.Quantity)
		if !ok {
			return nil, nil, fmt.Errorf("position %s has an out-of-domain quantity", p.InstrumentID)
		}
		value := new(big.Rat).Mul(qty, px)
		amount, ok := decutil.ToProtoScaled(value)
		if !ok {
			return nil, nil, fmt.Errorf("position %s is not representable at any exponent", p.InstrumentID)
		}
		marked[i] = &commonpb.Money{Amount: amount, CurrencyCode: b.BaseCurrency}
		// SIGNED, NOT ABSOLUTE. Equity nets a short against the cash it raised; it
		// is gross EXPOSURE that takes magnitudes, and conflating the two is how a
		// levered book reads as flat.
		equity.Add(equity, value)
	}
	return equity, marked, nil
}
