// Package tca measures what an order actually cost (#436).
//
// It answers the two questions an allocator asks and this platform previously
// could not: WHAT DID TRADING COST US, and WHICH VENUE SHOULD WE HAVE USED.
// Post-trade had confirmation, fails and settlement, and no cost measurement at
// all — so a strategy that is right and executes badly was indistinguishable
// from a strategy that is simply wrong.
//
// # It measures; it decides nothing
//
// Nothing here routes, sizes or refuses an order. That matters for the same
// reason it matters in internal/risk/unwind: a measurement that can also act is
// a measurement people stop trusting. #437 (venue ranking) and #435 (execution
// algorithms) are the consumers, and both are better built ON a signal than
// alongside a guess.
//
// # Fees are part of the cost, not a footnote
//
// A cost measure that omits fees ranks a zero-fee venue with poor fills above a
// maker-rebate venue with good ones — and it will be believed, because it is the
// only number on the page. Fee is therefore a required input rather than an
// optional refinement, and an order whose fills carry fees in a currency this
// package cannot combine is refused rather than measured without them.
//
// # Exact, because these are prices and quantities
//
// *big.Rat throughout. A basis-point figure derived through float64 drifts, and a
// venue comparison decided by the fourth decimal of a bps number is exactly the
// kind of thing that gets decided by rounding.
package tca

import (
	"errors"
	"fmt"
	"math/big"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

var (
	// ErrNoArrivalMark is returned when the order carries no decision-time
	// benchmark. It is NOT a zero-cost order — it is an unmeasurable one, and
	// scoring it as zero would drag every venue average toward whichever venue
	// trades the instruments the price spine covers worst.
	ErrNoArrivalMark = errors.New("tca: order has no arrival mark, so its cost cannot be measured")
	// ErrNoFills is returned for an order that never traded. Nothing was bought,
	// so nothing was paid; a zero here would be a real datapoint claiming a
	// perfect execution that never happened.
	ErrNoFills = errors.New("tca: order has no fills, so there is no execution to measure")
	// ErrMixedFeeCurrency is returned when fills carry fees in more than one
	// currency. Summing them would add euros to dollars and report the total as
	// cost; refusing is the only honest answer without an FX rate this package
	// deliberately does not have.
	ErrMixedFeeCurrency = errors.New("tca: fills carry fees in more than one currency")
)

// Result is one order's realized cost.
//
// Every figure is SIGNED FROM THE FUND'S POINT OF VIEW: positive means the order
// cost money relative to the benchmark, negative means it beat it. That
// convention is chosen once, here, because the alternative — "slippage" meaning
// opposite things for a buy and a sell — is how a cost report ends up averaging
// a good sell against a bad buy to zero.
type Result struct {
	OrderID    string
	Venue      string
	Instrument string

	// FilledQuantity and NotionalTraded are the size this measurement covers.
	// A partially-filled order is measured on what actually traded, because that
	// is what was paid for; the unfilled remainder has no cost.
	FilledQuantity *big.Rat
	NotionalTraded *big.Rat

	// AveragePrice is the quantity-weighted mean fill price, exclusive of fees.
	AveragePrice *big.Rat
	// ArrivalPrice is the benchmark: the market at the moment of the decision.
	ArrivalPrice *big.Rat

	// Fees is the total fee paid, in FeeCurrency.
	Fees        *big.Rat
	FeeCurrency string

	// ShortfallBps is implementation shortfall against arrival, in basis points
	// of arrival notional, INCLUDING fees. This is the headline number.
	ShortfallBps *big.Rat
	// PriceShortfallBps is the same measure EXCLUDING fees, so a venue's
	// execution quality and its fee schedule can be told apart. A venue can be
	// good at one and bad at the other, and a single blended figure hides which.
	PriceShortfallBps *big.Rat
}

// Measure computes an order's realized cost from its terminal state and fills.
//
// THE ARRIVAL MARK COMES FROM THE ORDER, not from a price looked up now. That is
// the whole reason it is stamped at admission: a mark read at measurement time
// is a mark from after this order moved the market, and using it would bias every
// result flatter, always in the flattering direction.
func Measure(st *orderpb.OrderState, fills []*orderpb.Fill) (Result, error) {
	if st == nil {
		return Result{}, ErrNoFills
	}
	arrival := dec.FromProto(st.GetArrivalPrice())
	if st.GetArrivalPrice() == nil || arrival.Sign() <= 0 {
		return Result{}, fmt.Errorf("%w: order %s", ErrNoArrivalMark, st.GetOrderId())
	}

	qty, notional := new(big.Rat), new(big.Rat)
	fees := new(big.Rat)
	feeCcy := ""
	var venue string

	for _, f := range fills {
		if f == nil {
			continue
		}
		q := dec.FromProto(f.GetQuantity())
		if q.Sign() <= 0 {
			continue // a fill that moved nothing contributes nothing
		}
		p := dec.FromProto(f.GetPrice())
		qty.Add(qty, q)
		notional.Add(notional, new(big.Rat).Mul(q, p))
		if venue == "" {
			venue = f.GetVenue()
		}

		if m := f.GetFee(); m != nil {
			ccy := m.GetCurrencyCode()
			amt := dec.FromProto(m.GetAmount())
			if amt.Sign() != 0 || ccy != "" {
				if feeCcy == "" {
					feeCcy = ccy
				} else if ccy != "" && ccy != feeCcy {
					// Adding euros to dollars and reporting the total as cost is
					// worse than refusing: the number would look right.
					return Result{}, fmt.Errorf("%w: order %s has fees in %q and %q",
						ErrMixedFeeCurrency, st.GetOrderId(), feeCcy, ccy)
				}
				fees.Add(fees, amt)
			}
		}
	}
	if qty.Sign() == 0 {
		return Result{}, fmt.Errorf("%w: order %s", ErrNoFills, st.GetOrderId())
	}

	avg := new(big.Rat).Quo(notional, qty)

	// THE SIGN IS THE HALF THAT IS EASY TO GET WRONG AND IMPOSSIBLE TO NOTICE.
	// A buy costs money when it pays ABOVE arrival; a sell costs money when it
	// receives BELOW. Folding both into "positive is bad" once, here, is what
	// stops a report averaging a good sell against a bad buy to zero.
	diff := new(big.Rat).Sub(avg, arrival)
	if st.GetSide() == orderpb.Side_SIDE_SELL {
		diff.Neg(diff)
	}
	// Cost in currency, then in basis points of the ARRIVAL notional — the
	// benchmark's own scale, not the achieved one. Dividing by what was actually
	// paid would make a worse execution look proportionally smaller.
	arrivalNotional := new(big.Rat).Mul(qty, arrival)
	priceCost := new(big.Rat).Mul(diff, qty)

	tenThousand := big.NewRat(10000, 1)
	priceBps := new(big.Rat).Quo(priceCost, arrivalNotional)
	priceBps.Mul(priceBps, tenThousand)

	totalCost := new(big.Rat).Add(priceCost, fees)
	totalBps := new(big.Rat).Quo(totalCost, arrivalNotional)
	totalBps.Mul(totalBps, tenThousand)

	if venue == "" {
		venue = st.GetVenue()
	}
	return Result{
		OrderID:           st.GetOrderId(),
		Venue:             venue,
		Instrument:        st.GetInstrumentId(),
		FilledQuantity:    qty,
		NotionalTraded:    notional,
		AveragePrice:      avg,
		ArrivalPrice:      arrival,
		Fees:              fees,
		FeeCurrency:       feeCcy,
		ShortfallBps:      totalBps,
		PriceShortfallBps: priceBps,
	}, nil
}

// SlippageVsVWAPBps compares the achieved average price against an interval VWAP
// — "did we trade worse than the market did over the order's life".
//
// A DIFFERENT QUESTION FROM SHORTFALL, and both are worth having. Shortfall says
// what the decision-to-execution round trip cost; VWAP slippage says whether the
// execution was worse than the average participant's over the same window. An
// order can beat arrival because the market moved in its favour while still being
// worked badly, and only the second measure sees that.
//
// Returns ok=false when there is no usable benchmark. Unknown is not zero here
// either: a window with no volume is a window nobody traded in, and scoring it
// as zero slippage would report perfect execution against an absent market.
func SlippageVsVWAPBps(r Result, vwap *big.Rat, side orderpb.Side) (*big.Rat, bool) {
	if vwap == nil || vwap.Sign() <= 0 || r.AveragePrice == nil {
		return nil, false
	}
	diff := new(big.Rat).Sub(r.AveragePrice, vwap)
	if side == orderpb.Side_SIDE_SELL {
		diff.Neg(diff)
	}
	bps := new(big.Rat).Quo(diff, vwap)
	return bps.Mul(bps, big.NewRat(10000, 1)), true
}

// IntervalVWAP folds bars into a volume-weighted average price.
//
// WEIGHTED BY VOLUME, NOT A MEAN OF CLOSES. A plain average of bar closes gives a
// quiet minute the same weight as the one that traded the whole day's size, which
// is precisely backwards for a benchmark meant to represent what the average
// participant paid.
//
// ok=false when total volume is zero — no trading happened, so there is no
// average participant to compare against, and inventing one would compare an
// execution to a market that was not there.
func IntervalVWAP(prices, volumes []*big.Rat) (*big.Rat, bool) {
	if len(prices) != len(volumes) {
		return nil, false
	}
	num, den := new(big.Rat), new(big.Rat)
	for i, p := range prices {
		v := volumes[i]
		if p == nil || v == nil || v.Sign() <= 0 {
			continue
		}
		num.Add(num, new(big.Rat).Mul(p, v))
		den.Add(den, v)
	}
	if den.Sign() == 0 {
		return nil, false
	}
	return new(big.Rat).Quo(num, den), true
}

// ToDecimal renders an exact figure for a FACT or a store, refusing rather than
// rounding. dec.ToProtoScaled preserves magnitude by rescaling; a value it cannot
// represent at any exponent is not something to write down approximately.
func ToDecimal(r *big.Rat) (*commonpb.Decimal, bool) {
	if r == nil {
		return nil, false
	}
	return dec.ToProtoScaled(r)
}
