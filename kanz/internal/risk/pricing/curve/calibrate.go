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
type QuoteSource interface {
	RateQuotes(ctx context.Context, currency string, asOf time.Time) ([]RateQuote, error)
}

// Calibrator ties the seam together: pull quotes, calibrate, publish the curve
// point-in-time. The composition root drives Refresh on its schedule (nightly
// close + intraday); the Store then resolves the curve at any as_of for the
// FI-01d CurveProvider seam.
type Calibrator struct {
	Source QuoteSource
	Store  *Store
	Interp Interpolation
}

// Refresh calibrates the currency's curve from quotes as of asOf and publishes
// it to the store, returning the calibrated curve. A source error or an
// uncalibratable quote set leaves the store unchanged (the previous curve keeps
// serving — no silent overwrite with garbage).
func (cal *Calibrator) Refresh(ctx context.Context, currency string, asOf time.Time) (*Curve, error) {
	quotes, err := cal.Source.RateQuotes(ctx, currency, asOf)
	if err != nil {
		return nil, fmt.Errorf("curve: quote source for %s: %w", currency, err)
	}
	c, err := Calibrate(quotes, cal.Interp)
	if err != nil {
		return nil, err
	}
	cal.Store.Put(currency, asOf, c)
	return c, nil
}
