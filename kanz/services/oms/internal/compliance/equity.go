package compliance

import (
	"fmt"
	"math/big"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
)

// markEquity turns the OMS's cost-basis book into a MARKED book with a real
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
func markEquity(b *comp.Book, marks MarkSource) {
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
	amount, ok := dec.ToProtoScaled(equity)
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
	b.NAVBasis = comp.NAVBasisEquity
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
// meaningless — the netting error dec.MoneyIn exists to stop.
func equityFromMarks(b *comp.Book, marks MarkSource) (*big.Rat, []*commonpb.Money, error) {
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
	equity, err := dec.MoneyIn(b.Cash, b.BaseCurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("cash cannot be netted into equity: %w", err)
	}
	equity = new(big.Rat).Set(equity)

	marked := make([]*commonpb.Money, len(b.Positions))
	for i, p := range b.Positions {
		if p.Quantity == nil {
			return nil, nil, fmt.Errorf("position %s carries no quantity, so it cannot be valued", p.InstrumentID)
		}
		// A POSITION ALREADY IN ANOTHER CURRENCY FALSIFIES THE ASSUMPTION ABOVE.
		// A nil or zero MarketValue is not that — it is a holding the book has not
		// priced, which is exactly what a mark is for.
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
		qty, ok := dec.FromProtoChecked(p.Quantity)
		if !ok {
			return nil, nil, fmt.Errorf("position %s has an out-of-domain quantity", p.InstrumentID)
		}
		value := new(big.Rat).Mul(qty, px)
		amount, ok := dec.ToProtoScaled(value)
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
