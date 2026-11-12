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
// sorted continuously compounded zero pillars; on a swaps-only annual strip it
// agrees with Bootstrap within binary64 error (the consistency test).
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
// unsolvable, unconverged or unrepresentable quote).
var ErrCalibrate = errors.New("curve: cannot calibrate quotes")

// Bound the annual fixed-leg work, not economic rate signs or maturities of
// directly supplied zero curves. A malformed swap must not allocate forever.
const maxCalibrationCoupons = 4096
const maxCalibrationEvaluations = 256
const calibrationEpsilon = 0x1p-52

func validRateQuote(q RateQuote) bool {
	return finite(q.Tenor, q.Span, q.Value, q.end()) &&
		q.Kind >= Deposit && q.Kind <= Swap &&
		q.Tenor >= 0 && (q.Kind == Future || q.Tenor > 0) &&
		q.Span >= 0 && (q.Kind != Swap || q.Tenor <= maxCalibrationCoupons)
}

// QuoteRate reprices the declared simple deposit/forward or annual par quote.
// It uses log-discount differences and expm1, so a tiny nonzero quote is not
// erased by subtracting rounded discount factors. Unavailable is NaN.
func (c *Curve) QuoteRate(q RateQuote) float64 {
	if !c.usable() || !validRateQuote(q) {
		return math.NaN()
	}
	switch q.Kind {
	case Deposit:
		return finiteOrNaN(math.Expm1(c.Zero(q.Tenor)*q.Tenor) / q.Tenor)
	case Future:
		return finiteOrNaN(math.Expm1(c.Forward(q.Tenor, q.end())*(q.end()-q.Tenor)) / q.span())
	case Swap:
		return c.ParRate(annualCouponTimes(q.Tenor))
	default:
		return math.NaN()
	}
}

func quoteResidualFits(got, want float64) bool {
	// Rate fractions: 128 binary64 epsilons relative to this quote, with no
	// absolute floor that could silently accept an erased near-zero input.
	return finite(got, want) && math.Abs(got-want) <= 128*calibrationEpsilon*math.Abs(want)
}

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
// Every input is repriced before returning; a failed residual check returns no
// curve, so Refresh cannot record, install or capture a failed calibration.
func Calibrate(quotes []RateQuote, interp Interpolation) (*Curve, error) {
	if len(quotes) == 0 {
		return nil, fmt.Errorf("%w: no quotes", ErrCalibrate)
	}
	if !validInterpolation(interp) {
		return nil, fmt.Errorf("%w: unsupported interpolation", ErrCalibrate)
	}
	for _, q := range quotes {
		// Validate before sorting or generating annual coupon dates. A malformed
		// tenor must not turn an unknown input into an unbounded allocation.
		if !validRateQuote(q) {
			return nil, fmt.Errorf("%w: invalid or over-budget quote", ErrCalibrate)
		}
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
		var z float64
		switch q.Kind {
		case Deposit, Future:
			period, logStart := q.Tenor, 0.0
			if q.Kind == Future {
				// A later pillar must not change the start discount used by the
				// closed form. Gaps require a different calibration equation.
				if len(b.tenors) == 0 || q.Tenor > prevEnd {
					return nil, fmt.Errorf("%w: future has no fixed start discount", ErrCalibrate)
				}
				period = q.span()
				logStart = b.Zero(q.Tenor) * q.Tenor
			}
			u := q.Value * period
			if !finite(u, logStart) || u <= -1 {
				return nil, fmt.Errorf("%w: invalid simple-rate discount", ErrCalibrate)
			}
			z = (logStart + math.Log1p(u)) / end
		case Swap:
			var err error
			z, err = solveSwapPillar(b, q)
			if err != nil {
				return nil, err
			}
		}
		df := math.Exp(-z * end)
		if !finite(z, df) || df <= 0 {
			return nil, fmt.Errorf("%w: unavailable discount at %.4g", ErrCalibrate, end)
		}
		b.tenors = append(b.tenors, end)
		b.zeros = append(b.zeros, z)
		prevEnd = end
	}
	for _, q := range qs {
		if !quoteResidualFits(b.QuoteRate(q), q.Value) {
			return nil, fmt.Errorf("%w: quote repricing residual at %.4g exceeds budget", ErrCalibrate, q.end())
		}
	}
	return b, nil
}

// solveSwapPillar retains the existing [-0.5, 1] zero-rate domain. It brackets
// from zero and the quote's scale, then requires both a narrow interval and a
// small repricing residual. Exhaustion or nonfinite evaluation never succeeds.
func solveSwapPillar(b *Curve, q RateQuote) (float64, error) {
	return solveSwapPillarWithBudget(b, q, maxCalibrationEvaluations)
}

func solveSwapPillarWithBudget(b *Curve, q RateQuote, budget int) (float64, error) {
	times := annualCouponTimes(q.Tenor)
	trial := &Curve{
		tenors: append(append([]float64{}, b.tenors...), q.Tenor),
		zeros:  append(append([]float64{}, b.zeros...), 0),
		interp: b.interp,
	}
	evaluate := func(z float64) (float64, error) {
		if budget <= 0 {
			return 0, fmt.Errorf("%w: swap evaluation budget exhausted", ErrCalibrate)
		}
		budget--
		trial.zeros[len(trial.zeros)-1] = z
		residual := trial.ParRate(times) - q.Value
		if !finite(residual) {
			return 0, fmt.Errorf("%w: nonfinite swap evaluation", ErrCalibrate)
		}
		return residual, nil
	}
	f0, err := evaluate(0)
	if err != nil {
		return 0, err
	}
	if q.Value == 0 {
		if f0 != 0 {
			return 0, fmt.Errorf("%w: zero swap does not reprice", ErrCalibrate)
		}
		return 0, nil
	}
	bound := 1.0
	if q.Value < 0 {
		bound = -0.5
	}
	edge := math.Copysign(math.Min(2*math.Abs(q.Value), math.Abs(bound)), bound)
	var fe float64
	for {
		fe, err = evaluate(edge)
		if err != nil {
			return 0, err
		}
		if (bound > 0 && fe >= 0) || (bound < 0 && fe <= 0) {
			break
		}
		if edge == bound {
			return 0, fmt.Errorf("%w: swap root outside supported domain", ErrCalibrate)
		}
		edge = math.Copysign(math.Min(2*math.Abs(edge), math.Abs(bound)), bound)
	}
	lo, hi := math.Min(0, edge), math.Max(0, edge)
	flo, fhi := f0, fe
	if edge < 0 {
		flo, fhi = fe, f0
	}
	for {
		mid := lo + (hi-lo)/2
		if mid == lo || mid == hi {
			return 0, fmt.Errorf("%w: swap bracket stagnated", ErrCalibrate)
		}
		f, err := evaluate(mid)
		if err != nil {
			return 0, err
		}
		if f < 0 {
			lo, flo = mid, f
		} else {
			hi, fhi = mid, f
		}
		best, residual := lo, flo
		if math.Abs(fhi) < math.Abs(flo) {
			best, residual = hi, fhi
		}
		if hi-lo <= 64*calibrationEpsilon*math.Max(math.Abs(lo), math.Abs(hi)) &&
			math.Abs(residual) <= 64*calibrationEpsilon*math.Abs(q.Value) && best != 0 {
			return best, nil
		}
	}
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
	// OnCaptured observes successful authoritative installation only. It must
	// enqueue nonblocking; shadow execution belongs outside Refresh. It receives
	// detached data and cannot replace the installed curve or return a refusal.
	OnCaptured func(CalibrationCapture)
	Source     QuoteSource
	Store      *Store
	Interp     Interpolation

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

	// OnCalibrated commits the artifact before it becomes visible to pricing.
	// Failure leaves the previous curve in service and refuses this refresh.
	// A retained artifact need not have priced a book, but every curve that did
	// price one must be retained. Nil is for callers with no durability contract.
	OnCalibrated func(ctx context.Context, currency string, asOf time.Time, c *Curve) error
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
	if cal.OnCalibrated != nil {
		if err := cal.OnCalibrated(ctx, currency, asOf, stored); err != nil {
			return nil, fmt.Errorf("curve: record %s: %w", currency, err)
		}
	}
	cal.Store.Put(currency, asOf, stored)
	if cal.OnCaptured != nil && strip.Capture != nil {
		cal.OnCaptured(CalibrationCapture{Currency: currency, RequestedAsOf: asOf, CapturedAt: strip.Capture.CapturedAt, Interp: cal.Interp, Strip: strip.clone(), Tenors: stored.Tenors(), Zeros: stored.Zeros()})
	}
	return c, nil
}
