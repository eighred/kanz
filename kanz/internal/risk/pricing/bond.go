package pricing

import (
	"math"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// Bond analytics (FI-01c): price/yield, accrued interest, and the rate-risk
// measures — Macaulay/modified duration, convexity, DV01, and curve-based
// effective + key-rate durations. Float internals like the rest of pricing;
// money lands as Decimal at the measure edge (compute/fi.go, FI-01d).
//
// Yield-based analytics use periodic compounding at the coupon frequency (the
// market convention: a semiannual bond quotes a semiannual-compounded yield).
// Curve-based pricing discounts each cashflow off a curve.Curve — the same
// continuously-compounded discount basis the option pricers use.

// Bond is a fixed-rate (or zero-coupon) bullet bond — the float working shape
// behind reference.v1.BondTerms.
type Bond struct {
	Face       float64   // redemption / par amount
	CouponRate float64   // annualized coupon (0 ⇒ zero-coupon)
	Frequency  int       // coupons per year (0 ⇒ zero-coupon, annual compounding)
	Issue      time.Time // accrual start
	Maturity   time.Time // redemption
	DayCount   DayCount  // accrual convention
}

// BondRisk bundles the yield-based price and rate-risk measures of a bond,
// computed together in one pass over its cashflows.
type BondRisk struct {
	DirtyPrice       float64 // PV of future cashflows at the yield
	MacaulayDuration float64 // cashflow-weighted mean time (years)
	ModifiedDuration float64 // −(1/P)·dP/dy
	Convexity        float64 // (1/P)·d²P/dy²
	DV01             float64 // price change per 1bp yield move (absolute)
}

// KeyRate is one key-rate duration: the sensitivity to a localized 1bp bump of
// the curve at Tenor (years).
type KeyRate struct {
	Tenor    float64
	Duration float64
}

type cashflow struct {
	date   time.Time
	amount float64
}

// cFreq is the compounding frequency: the coupon frequency, or 1 (annual) for a
// zero-coupon bond.
func (b Bond) cFreq() int {
	if b.Frequency <= 0 {
		return 1
	}
	return b.Frequency
}

// schedule returns the coupon payment dates ascending, ending at maturity. A
// zero-coupon bond has only the redemption date.
func (b Bond) schedule() []time.Time {
	if b.Frequency <= 0 || b.CouponRate == 0 {
		return []time.Time{b.Maturity}
	}
	months := 12 / b.Frequency
	var dates []time.Time
	for d := b.Maturity; d.After(b.Issue); d = d.AddDate(0, -months, 0) {
		dates = append(dates, d)
	}
	for i, j := 0, len(dates)-1; i < j; i, j = i+1, j-1 {
		dates[i], dates[j] = dates[j], dates[i]
	}
	return dates
}

// couponAmount is the per-period coupon payment (0 for a zero-coupon bond).
func (b Bond) couponAmount() float64 {
	if b.Frequency <= 0 || b.CouponRate == 0 {
		return 0
	}
	return b.Face * b.CouponRate / float64(b.Frequency)
}

// futureCashflows returns the cashflows strictly after settle, with the face
// added to the final (maturity) payment.
func (b Bond) futureCashflows(settle time.Time) []cashflow {
	sched := b.schedule()
	coupon := b.couponAmount()
	cfs := make([]cashflow, 0, len(sched))
	for i, d := range sched {
		if !d.After(settle) {
			continue
		}
		amt := coupon
		if i == len(sched)-1 {
			amt += b.Face
		}
		cfs = append(cfs, cashflow{d, amt})
	}
	return cfs
}

// PriceFromYield returns the dirty price (PV of future cashflows) at yield y,
// periodic-compounded at the coupon frequency.
func (b Bond) PriceFromYield(settle time.Time, y float64) float64 {
	f := float64(b.cFreq())
	price := 0.0
	for _, cf := range b.futureCashflows(settle) {
		n := YearFraction(settle, cf.date, b.DayCount) * f
		price += cf.amount * math.Pow(1+y/f, -n)
	}
	return price
}

// Risk computes the dirty price and the yield-based rate-risk measures at yield
// y in a single cashflow pass.
func (b Bond) Risk(settle time.Time, y float64) BondRisk {
	f := float64(b.cFreq())
	base := 1 + y/f
	var price, dPrice, d2Price float64
	for _, cf := range b.futureCashflows(settle) {
		t := YearFraction(settle, cf.date, b.DayCount)
		n := t * f
		pv := cf.amount * math.Pow(base, -n)
		price += pv
		dPrice += pv * (-t) / base                              // dP/dy
		d2Price += pv * (n * (n + 1) / (f * f)) / (base * base) // d²P/dy²
	}
	if price == 0 {
		return BondRisk{}
	}
	mod := -dPrice / price
	return BondRisk{
		DirtyPrice:       price,
		ModifiedDuration: mod,
		MacaulayDuration: mod * base,
		Convexity:        d2Price / price,
		DV01:             math.Abs(dPrice) * 1e-4,
	}
}

// YieldFromPrice solves for the yield that reprices the bond to dirtyPrice, by
// bisection (price is monotone decreasing in yield). Returns the yield in
// [-0.5, 2.0]; a price outside that band clamps to the nearest endpoint.
func (b Bond) YieldFromPrice(settle time.Time, dirtyPrice float64) float64 {
	lo, hi := -0.5, 2.0
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if b.PriceFromYield(settle, mid) > dirtyPrice {
			lo = mid // price too high ⇒ yield too low
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// AccruedInterest is the coupon accrued from the last coupon date (or issue) to
// settle, pro-rated by the day-count fraction of the current period. Zero for a
// zero-coupon bond or a settle at/after maturity.
func (b Bond) AccruedInterest(settle time.Time) float64 {
	coupon := b.couponAmount()
	if coupon == 0 {
		return 0
	}
	prev := b.Issue
	var next time.Time
	found := false
	for _, d := range b.schedule() {
		if d.After(settle) {
			next, found = d, true
			break
		}
		prev = d
	}
	if !found {
		return 0
	}
	period := YearFraction(prev, next, b.DayCount)
	if period <= 0 {
		return 0
	}
	return coupon * YearFraction(prev, settle, b.DayCount) / period
}

// CleanPrice is the dirty price at yield y less accrued interest — the quoted
// price convention.
func (b Bond) CleanPrice(settle time.Time, y float64) float64 {
	return b.PriceFromYield(settle, y) - b.AccruedInterest(settle)
}

// PriceFromCurve discounts each future cashflow off the curve (continuous DF,
// ACT/365F curve time) — the no-single-yield price used for curve scenarios and
// effective/key-rate durations.
func (b Bond) PriceFromCurve(settle time.Time, c *curve.Curve) float64 {
	price := 0.0
	for _, cf := range b.futureCashflows(settle) {
		t := YearFraction(settle, cf.date, Actual365Fixed)
		price += cf.amount * c.Discount(t)
	}
	return price
}

// EffectiveDuration is the curve-based duration: the symmetric 1bp parallel-shift
// price sensitivity, −(P₊ − P₋)/(2·P·Δy). Captures the full term structure where
// the single-yield ModifiedDuration assumes a flat yield.
func (b Bond) EffectiveDuration(settle time.Time, c *curve.Curve) float64 {
	base := b.PriceFromCurve(settle, c)
	if base == 0 {
		return 0
	}
	up := b.PriceFromCurve(settle, curve.Parallel{Bp: 1}.Apply(c))
	down := b.PriceFromCurve(settle, curve.Parallel{Bp: -1}.Apply(c))
	return -(up - down) / (2 * base * 1e-4)
}

// CurveRisk is the curve-based price and rate-risk of a bond, from symmetric 1bp
// parallel bumps — the no-single-yield analog of Risk used by the FI risk
// measures (FI-01d) so reporting prices off the same curve scenarios do.
type CurveRisk struct {
	DirtyPrice        float64
	EffectiveDuration float64 // −(P₊−P₋)/(2·P·Δy)
	Convexity         float64 // (P₊+P₋−2P)/(P·Δy²)
	DV01              float64 // |P₊−P₋|/2 per 1bp (price units, per bond)
}

// CurveRisk computes the curve-based duration/convexity/DV01 in one set of
// reprices (base + ±1bp parallel).
func (b Bond) CurveRisk(settle time.Time, c *curve.Curve) CurveRisk {
	const dy = 1e-4
	base := b.PriceFromCurve(settle, c)
	if base == 0 {
		return CurveRisk{}
	}
	up := b.PriceFromCurve(settle, curve.Parallel{Bp: 1}.Apply(c))
	down := b.PriceFromCurve(settle, curve.Parallel{Bp: -1}.Apply(c))
	return CurveRisk{
		DirtyPrice:        base,
		EffectiveDuration: -(up - down) / (2 * base * dy),
		Convexity:         (up + down - 2*base) / (base * dy * dy),
		DV01:              math.Abs(up-down) / 2,
	}
}

// KeyRateDurations returns the sensitivity to a localized 1bp bump of the curve
// at each of its pillars. The bumps tile the curve, so the key-rate durations
// sum (approximately) to the effective duration — the FI-01f decomposition check
// and the input to a hedging/bucketed-DV01 view.
func (b Bond) KeyRateDurations(settle time.Time, c *curve.Curve) []KeyRate {
	base := b.PriceFromCurve(settle, c)
	tenors := c.Tenors()
	out := make([]KeyRate, len(tenors))
	if base == 0 {
		for i, t := range tenors {
			out[i] = KeyRate{Tenor: t}
		}
		return out
	}
	for i, t := range tenors {
		p := b.PriceFromCurve(settle, c.WithPillarBump(i, 1e-4))
		out[i] = KeyRate{Tenor: t, Duration: -(p - base) / base / 1e-4}
	}
	return out
}
