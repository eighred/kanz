package curve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Live rate-curve calibration (PARITY-03a). Bootstrap (FI-01b) builds a curve
// from a homogeneous par strip; a live curve is calibrated from the mixed
// money-market universe — deposits on the short end, rate futures in the belly,
// par swaps on the long end. Calibrate solves the mixed set sequentially into
// zero pillars and hands them to NewZeroCurve; on a swaps-only annual strip it
// reproduces Bootstrap exactly (the consistency test).
//
// Quotes reach the calibrator through the QuoteSource seam — the live surface
// (the PARITY-01h feed.Snapshot) is market-data-service-internal, so the
// composition root adapts it to QuoteSource (the MODEL-01c closure-injection
// stance); this package stays free of feed/bus imports.

// QuoteKind identifies the market instrument behind a RateQuote.
type QuoteKind int

const (
	// Deposit is a simple-interest money-market deposit: Value is the deposit
	// rate, Tenor the maturity. DF(T) = 1/(1+r·T) — a single flow at maturity.
	Deposit QuoteKind = iota
	// Future is a rate future / FRA: Value is the implied simple FORWARD rate
	// over [Tenor, Tenor+Span] (use FutureRateFromPrice for a price quote);
	// Span defaults to 0.25 (the 3M contract) when zero.
	Future
	// Swap is a par swap: Value is the fixed rate, annual fixed leg (the
	// ParRate convention), Tenor the swap maturity.
	Swap
)

// RateQuote is one calibration instrument. Tenors are in years; Value is a
// decimal rate (0.045 = 4.5%).
type RateQuote struct {
	Kind  QuoteKind
	Tenor float64 // deposit/swap maturity; future period START
	Span  float64 // Future only: underlying period length (0 ⇒ 0.25)
	Value float64
}

// FutureRateFromPrice converts an IMM-style futures price quote (e.g. 95.5) to
// the implied simple forward rate ((100−P)/100) Calibrate expects.
func FutureRateFromPrice(price float64) float64 { return (100 - price) / 100 }

// ErrCalibrate is returned when a quote set cannot be calibrated (empty set,
// duplicate/non-positive pillars, a future with no short-end anchor, or an
// unsolvable/arbitrageable quote).
var ErrCalibrate = errors.New("curve: cannot calibrate quotes")

// end returns the pillar tenor a quote resolves: instrument maturity.
func (q RateQuote) end() float64 {
	if q.Kind == Future {
		return q.Tenor + q.span()
	}
	return q.Tenor
}

func (q RateQuote) span() float64 {
	if q.Span > 0 {
		return q.Span
	}
	return 0.25
}

// Calibrate bootstraps a zero curve from a mixed deposit/future/swap quote set.
// Quotes need not be pre-sorted; they are solved in ascending maturity order,
// each pillar off the curve built so far (interpolated per interp). Deposits
// and futures solve in closed form; a swap pillar is solved by bisection so the
// curve's ParRate reproduces the quote (the FI-01f round-trip property).
func Calibrate(quotes []RateQuote, interp Interpolation) (*Curve, error) {
	if len(quotes) == 0 {
		return nil, fmt.Errorf("%w: no quotes", ErrCalibrate)
	}
	qs := make([]RateQuote, len(quotes))
	copy(qs, quotes)
	sort.Slice(qs, func(i, j int) bool { return qs[i].end() < qs[j].end() })

	b := &Curve{interp: interp} // pillars accumulate in place
	prevEnd := 0.0
	for _, q := range qs {
		end := q.end()
		if end <= 0 || end <= prevEnd {
			return nil, fmt.Errorf("%w: pillar tenors must be strictly ascending and positive (%.4g after %.4g)", ErrCalibrate, end, prevEnd)
		}
		var df float64
		switch q.Kind {
		case Deposit:
			df = 1 / (1 + q.Value*q.Tenor)
		case Future:
			if len(b.tenors) == 0 {
				return nil, fmt.Errorf("%w: future at %.4g has no short-end anchor", ErrCalibrate, q.Tenor)
			}
			df = b.Discount(q.Tenor) / (1 + q.Value*q.span())
		case Swap:
			z, err := solveSwapPillar(b, q)
			if err != nil {
				return nil, err
			}
			df = math.Exp(-z * end)
		default:
			return nil, fmt.Errorf("%w: unknown quote kind %d", ErrCalibrate, q.Kind)
		}
		if df <= 0 || math.IsNaN(df) {
			return nil, fmt.Errorf("%w: non-positive discount factor at %.4g", ErrCalibrate, end)
		}
		b.tenors = append(b.tenors, end)
		b.zeros = append(b.zeros, -math.Log(df)/end)
		prevEnd = end
	}
	return NewZeroCurve(b.tenors, b.zeros, Continuous, interp)
}

// solveSwapPillar finds the zero rate at the swap's maturity that makes the
// curve-so-far (plus the trial pillar) reprice the swap to par: ParRate over
// annual coupon dates equals the quote. Bisection — ParRate is monotone
// increasing in the final zero, so one sign change brackets the root.
func solveSwapPillar(b *Curve, q RateQuote) (float64, error) {
	times := annualCouponTimes(q.Tenor)
	parErr := func(z float64) float64 {
		trial := &Curve{
			tenors: append(append([]float64{}, b.tenors...), q.Tenor),
			zeros:  append(append([]float64{}, b.zeros...), z),
			interp: b.interp,
		}
		return trial.ParRate(times) - q.Value
	}
	lo, hi := -0.5, 1.0
	fLo, fHi := parErr(lo), parErr(hi)
	if fLo > 0 || fHi < 0 || math.IsNaN(fLo) || math.IsNaN(fHi) {
		return 0, fmt.Errorf("%w: swap quote %.4g at %.4gy is unsolvable", ErrCalibrate, q.Value, q.Tenor)
	}
	for i := 0; i < 200 && hi-lo > 1e-14; i++ {
		mid := (lo + hi) / 2
		if parErr(mid) < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, nil
}

// annualCouponTimes are the fixed-leg payment dates of a par swap maturing at
// tenor: 1, 2, …, with a final (possibly stub) payment at tenor itself.
func annualCouponTimes(tenor float64) []float64 {
	var ts []float64
	for t := 1.0; t < tenor-1e-9; t++ {
		ts = append(ts, t)
	}
	return append(ts, tenor)
}

// QuoteSource supplies the calibration quote set for a currency as of a point
// in time — the calibrator-local seam the live quote surface (feed.Snapshot)
// adapts to at the composition root.
//
// IT RETURNS A Strip, NOT A []RateQuote, AND THAT IS THE POINT (#908). A source
// resolves a CONFIGURED set of instruments and yields whatever subset had a
// usable price; returning only the subset made a short strip unreportable by
// anything downstream, because the size of the set it came from had already
// been thrown away at this line. Carrying the coverage in the same return value
// is what makes it impossible to take the quotes without it. A source that
// genuinely has no configured set (a fixed test vector) returns a zero
// StripCoverage, which reads as UNKNOWN rather than as complete.
type QuoteSource interface {
	RateQuotes(ctx context.Context, currency string, asOf time.Time) (Strip, error)
}

// Calibrator ties the seam together: pull quotes, calibrate, publish the curve
// point-in-time. The composition root drives Refresh on its schedule (nightly
// close + intraday); the Store then resolves the curve at any as_of for the
// FI-01d CurveProvider seam.
type Calibrator struct {
	Source QuoteSource
	Store  *Store
	Interp Interpolation

	// OnCoverage is called on EVERY refresh with what the source resolved,
	// before calibration is attempted. Optional; nil disables it.
	//
	// IT FIRES BEFORE Calibrate, AND ON FAILURES TOO, which is the whole reason
	// it exists rather than the caller reading Curve.StripCoverage off the
	// returned curve. The worst coverage — every configured instrument missing —
	// is exactly the case where Calibrate refuses and there IS no curve to read
	// it from, so a caller that learned coverage only from a successful refresh
	// would be blind in the one state that matters most. Refresh's error says
	// "no quotes"; only this says WHICH configured instruments produced it.
	//
	// IT FIRES ON COMPLETE STRIPS TOO, and that is not noise. A signal emitted
	// only when something is wrong makes "healthy" and "not reporting"
	// indistinguishable — the defect this seam exists to end, one level up. The
	// observer decides what to do with a complete report; it must not be the
	// thing that decides whether one exists.
	//
	// THIS IS A METRIC SEAM AND IT DOES NOT REPLACE Curve.StripCoverage, nor the
	// reverse — the same split FIProviders.OnSkip states. This one reaches an
	// operator watching the pod, at refresh time, whether or not a curve was
	// produced; the coverage on the curve reaches the caller pricing off it,
	// later, at an arbitrary as-of, through the point-in-time store.
	OnCoverage func(currency string, cov StripCoverage)

	// OnCalibrated is called with every curve that reaches the store, after the
	// Put that publishes it. Optional; nil disables it.
	//
	// IT IS THE DURABILITY SEAM, and it exists because the store is not one
	// (#1039). Store is in-memory and starts empty: a deploy, a KEDA scale event
	// or an OOM kill leaves it holding no versions at all, so the curve that
	// discounted a published DV01 is gone with the pod while the DV01 itself
	// lives 30 days on the risk.portfolio topic. The number survives its own
	// inputs, which is the state that makes "reproduce last Tuesday's valuation"
	// unanswerable.
	//
	// AFTER Put AND NOT BEFORE. The observer records what a reader can actually
	// resolve out of the store; recording a curve the store then rejected — or
	// recording before a panic between the two — would put an artifact in the
	// permanent record that never priced anything.
	//
	// SEPARATE FROM OnCoverage, which fires on every refresh INCLUDING the ones
	// that produced no curve. Coverage is a metric about the attempt; this is the
	// record of an artifact, and there is nothing to record when calibration
	// refused.
	OnCalibrated func(ctx context.Context, currency string, asOf time.Time, c *Curve)
}

// Refresh calibrates the currency's curve from quotes as of asOf and publishes
// it to the store, returning the calibrated curve. A source error or an
// uncalibratable quote set leaves the store unchanged (the previous curve keeps
// serving — no silent overwrite with garbage).
func (cal *Calibrator) Refresh(ctx context.Context, currency string, asOf time.Time) (*Curve, error) {
	strip, err := cal.Source.RateQuotes(ctx, currency, asOf)
	if err != nil {
		return nil, fmt.Errorf("curve: quote source for %s: %w", currency, err)
	}
	// Reported before Calibrate can refuse: see OnCoverage. A SOURCE error above
	// is deliberately not reported — the source could not say what it resolved,
	// so there is no coverage to state, and a fabricated zero would read as "the
	// whole strip is missing" when the truth is that nothing was looked at.
	if cal.OnCoverage != nil {
		cal.OnCoverage(currency, strip.Coverage)
	}
	c, err := Calibrate(strip.Quotes, cal.Interp)
	if err != nil {
		return nil, err
	}
	// Stamped before Put, so no reader can resolve this curve out of the
	// point-in-time store without the record of what it was built from.
	stored := c.withStripCoverage(strip.Coverage)
	cal.Store.Put(currency, asOf, stored)
	if cal.OnCalibrated != nil {
		cal.OnCalibrated(ctx, currency, asOf, stored)
	}
	return c, nil
}
