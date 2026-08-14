package tca

import (
	"errors"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// WHAT DID TRADING COST US (#436).
//
// Every expected value below is hand-worked in the comment above it and asserted
// as an EXACT rational. That is deliberate: a bps figure is small, and a wrong
// one is entirely plausible — the failure mode of a cost measure is not a crash,
// it is a believable number that flatters the venue nobody checks.

func rat(n int64) *big.Rat { return big.NewRat(n, 1) }

func d(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func order(side orderpb.Side, arrival *commonpb.Decimal) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
		Side: side, ArrivalPrice: arrival,
	}
}

func fill(qty, price int64, fee int64, ccy string) *orderpb.Fill {
	f := &orderpb.Fill{
		Quantity: d(qty, 0), Price: d(price, 0), Venue: "XBIN",
	}
	if ccy != "" {
		f.Fee = &commonpb.Money{Amount: d(fee, 0), CurrencyCode: ccy}
	}
	return f
}

// A BUY THAT PAID ABOVE ARRIVAL COST MONEY.
//
// Arrival 100. Two fills: 10 @ 101 and 10 @ 103.
//
//	filled       = 20
//	notional     = 10·101 + 10·103 = 2060
//	average      = 2060/20 = 102
//	arrival ntl  = 20·100 = 2000
//	price cost   = (102 − 100)·20 = 40
//	price bps    = 40/2000 · 10000 = 200
//	fees         = 2 + 2 = 4
//	total bps    = 44/2000 · 10000 = 220
func TestMeasure_BuyAboveArrivalIsAPositiveCost(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 101, 2, "USD"), fill(10, 103, 2, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.AveragePrice.RatString(); got != "102" {
		t.Errorf("average = %s, want 102", got)
	}
	if got := r.PriceShortfallBps.RatString(); got != "200" {
		t.Errorf("price shortfall = %s bps, want 200", got)
	}
	if got := r.ShortfallBps.RatString(); got != "220" {
		t.Errorf("total shortfall = %s bps, want 220 (200 price + 20 fees)", got)
	}
	if got := r.Fees.RatString(); got != "4" {
		t.Errorf("fees = %s, want 4", got)
	}
}

// A SELL BELOW ARRIVAL ALSO COSTS MONEY — the sign is folded once, here, so a
// report cannot average a good sell against a bad buy to zero.
//
// Arrival 100, sell 10 @ 98. price cost = (100 − 98)·10 = 20 over 1000 arrival
// notional = 200 bps.
func TestMeasure_SellBelowArrivalIsAlsoAPositiveCost(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_SELL, d(100, 0)),
		[]*orderpb.Fill{fill(10, 98, 0, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.PriceShortfallBps.RatString(); got != "200" {
		t.Fatalf("sell shortfall = %s bps, want +200. A sell that received BELOW arrival cost "+
			"money; if this is negative the sign convention is inverted and every mixed report "+
			"averages toward zero", got)
	}
}

// BEATING THE BENCHMARK IS NEGATIVE. Without this, "cost" would be an absolute
// value and a genuinely good execution would be indistinguishable from a bad one
// of the same size.
func TestMeasure_BeatingArrivalIsNegative(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 99, 0, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.PriceShortfallBps.RatString(); got != "-100" {
		t.Fatalf("shortfall = %s bps, want -100 — buying below arrival is a saving", got)
	}
}

// FEES ARE PART OF THE COST, and separable from it.
//
// This is the trap #436 names: a measure that omits fees ranks a zero-fee venue
// with poor fills above a maker-rebate venue with good ones. Both figures are
// reported so a venue's execution quality and its fee schedule can be told
// apart — a blended number hides which one is bad.
//
// A perfect fill at arrival, with a fee of 10 on an arrival notional of
// 10·100 = 1000. 10/1000 = 1%, so total = 100 bps while price shortfall is 0.
//
// I hand-worked this as 10 bps first and the test caught me: a fee of ten on a
// notional of one thousand is one percent, and one percent is a hundred basis
// points. Recorded because that is the exact size of error a cost report makes
// invisible — 10 and 100 are both entirely plausible on the page.
func TestMeasure_FeesAreCountedAndSeparable(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 100, 10, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.PriceShortfallBps.RatString(); got != "0" {
		t.Errorf("price shortfall = %s, want 0 — the fill was exactly at arrival", got)
	}
	if got := r.ShortfallBps.RatString(); got != "100" {
		t.Fatalf("total shortfall = %s bps, want 100. A cost measure that drops fees ranks a "+
			"zero-fee venue with poor fills above a maker-rebate venue with good ones", got)
	}
}

// A MAKER REBATE IS A NEGATIVE FEE and must reduce cost, not be treated as an
// absolute charge. Rebate of 5 on an arrival notional of 1000 = 50 bps received.
func TestMeasure_ARebateReducesCost(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 100, -5, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.ShortfallBps.RatString(); got != "-50" {
		t.Fatalf("total shortfall = %s bps, want -50 — a rebate is money received", got)
	}
}

// NO ARRIVAL MARK IS UNMEASURABLE, NOT ZERO-COST.
//
// Scoring these as zero would drag every venue average toward whichever venue
// trades the instruments the price spine covers worst — the opposite of what a
// venue comparison is for.
func TestMeasure_NoArrivalMarkIsRefused(t *testing.T) {
	_, err := Measure(order(orderpb.Side_SIDE_BUY, nil),
		[]*orderpb.Fill{fill(10, 101, 0, "USD")})
	if !errors.Is(err, ErrNoArrivalMark) {
		t.Fatalf("err = %v, want ErrNoArrivalMark — an order with no benchmark must be skipped, "+
			"never scored as zero cost", err)
	}
}

// AN UNFILLED ORDER HAS NO EXECUTION TO MEASURE. Nothing was bought, so nothing
// was paid; a zero here would claim a perfect execution that never happened.
func TestMeasure_NoFillsIsRefused(t *testing.T) {
	if _, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)), nil); !errors.Is(err, ErrNoFills) {
		t.Fatalf("err = %v, want ErrNoFills", err)
	}
}

// MIXED FEE CURRENCIES ARE REFUSED. Summing them would add euros to dollars and
// report the total as cost — a number that looks entirely right.
func TestMeasure_MixedFeeCurrenciesAreRefused(t *testing.T) {
	_, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 100, 1, "USD"), fill(10, 100, 1, "EUR")})
	if !errors.Is(err, ErrMixedFeeCurrency) {
		t.Fatalf("err = %v, want ErrMixedFeeCurrency — adding EUR to USD and calling it cost is "+
			"worse than refusing, because the total looks right", err)
	}
}

// A PARTIAL FILL IS MEASURED ON WHAT TRADED. The unfilled remainder was not paid
// for, so including it would dilute the cost of what actually executed.
func TestMeasure_PartialFillMeasuresOnlyWhatTraded(t *testing.T) {
	st := order(orderpb.Side_SIDE_BUY, d(100, 0))
	st.OrderedQuantity = d(100, 0) // asked for 100
	r, err := Measure(st, []*orderpb.Fill{fill(10, 110, 0, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.FilledQuantity.RatString(); got != "10" {
		t.Errorf("filled = %s, want 10", got)
	}
	// 10 bought at 110 against arrival 100 = 1000 bps, undiluted by the 90 never bought.
	if got := r.PriceShortfallBps.RatString(); got != "1000" {
		t.Fatalf("shortfall = %s bps, want 1000 — the unfilled remainder must not dilute the "+
			"cost of what actually traded", got)
	}
}

// EXACTNESS. A price a float64 cannot hold must survive the whole calculation:
// a venue comparison decided by the fourth decimal of a bps figure is exactly
// what rounding decides for you.
func TestMeasure_StaysExactWhereAFloatWouldNot(t *testing.T) {
	// arrival 3, fill 10 @ 1 — shortfall = (1−3)/3 · 10000 = −20000/3, a
	// non-terminating decimal.
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(3, 0)),
		[]*orderpb.Fill{fill(10, 1, 0, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.PriceShortfallBps.RatString(); got != "-20000/3" {
		t.Fatalf("shortfall = %s, want -20000/3 exactly — the calculation left the exact domain", got)
	}
}

// INTERVAL VWAP IS VOLUME-WEIGHTED, not a mean of closes.
//
// A plain average gives a quiet minute the same weight as the one that traded
// the day's size, which is backwards for a benchmark meant to represent what the
// average participant paid.
//
//	(100·1 + 200·9) / 10 = 1900/10 = 190   — NOT the plain mean of 150.
func TestIntervalVWAP_WeightsByVolume(t *testing.T) {
	got, ok := IntervalVWAP([]*big.Rat{rat(100), rat(200)}, []*big.Rat{rat(1), rat(9)})
	if !ok {
		t.Fatal("no VWAP from two bars with volume")
	}
	if got.RatString() != "190" {
		t.Fatalf("vwap = %s, want 190. A plain mean gives 150 and would treat a one-lot minute "+
			"as equal to a nine-lot one", got.RatString())
	}
}

// A WINDOW WITH NO VOLUME HAS NO BENCHMARK. Nobody traded, so there is no average
// participant to compare against; inventing one would score an execution against
// a market that was not there.
func TestIntervalVWAP_NoVolumeIsUnknownNotZero(t *testing.T) {
	if _, ok := IntervalVWAP([]*big.Rat{rat(100)}, []*big.Rat{rat(0)}); ok {
		t.Fatal("a zero-volume window produced a VWAP")
	}
}

// SLIPPAGE VS VWAP IS A DIFFERENT QUESTION FROM SHORTFALL, and this is the case
// that shows why both are needed: an order can BEAT arrival because the market
// moved in its favour while still being worked worse than everyone else.
//
// Arrival 100, bought 10 @ 95 — shortfall −500 bps, a saving. But the market's
// VWAP over the window was 90, so the execution was 500 bps WORSE than the
// average participant.
func TestSlippageVsVWAP_CatchesABadExecutionThatBeatArrival(t *testing.T) {
	r, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 95, 0, "USD")})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := r.PriceShortfallBps.RatString(); got != "-500" {
		t.Fatalf("shortfall = %s, want -500 (it beat arrival)", got)
	}

	slip, ok := SlippageVsVWAPBps(r, rat(90), orderpb.Side_SIDE_BUY)
	if !ok {
		t.Fatal("no slippage computed against a real VWAP")
	}
	// (95 − 90)/90 · 10000 = 50000/90 = 5000/9
	if slip.RatString() != "5000/9" {
		t.Fatalf("slippage = %s bps, want 5000/9 — positive, because it paid ABOVE the market's "+
			"own average while still beating arrival. Shortfall alone would call this a win",
			slip.RatString())
	}
}

// NO VWAP ⇒ NO SLIPPAGE, reported as absent rather than zero.
func TestSlippageVsVWAP_AbsentBenchmarkIsNotZero(t *testing.T) {
	r, _ := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)),
		[]*orderpb.Fill{fill(10, 95, 0, "USD")})
	if _, ok := SlippageVsVWAPBps(r, nil, orderpb.Side_SIDE_BUY); ok {
		t.Fatal("slippage reported against a nil VWAP")
	}
}
