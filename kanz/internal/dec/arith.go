package dec

import (
	"math"
	"math/big"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// DECIMAL ARITHMETIC THAT CANNOT WRAP.
//
// This file was internal/compliance/gate.go until #216. It is here because a
// THIRD copy of it had already been written by hand — internal/risk/compute —
// and that copy still multiplied raw int64 coefficients: a -20% stress on a
// $200,000 position returned -$24,467 instead of +$160,000, sign and all. The
// compliance gate had been repaired, dec.ToProtoScaled had been repaired, and
// the risk engine had not, because there was nothing for it to call.
//
// The analysis below is the compliance gate's, kept verbatim in substance
// because it is what justifies the shape. Read "the gate REFUSES the order" as
// "the caller REFUSES the value" — the operational consequence differs per
// caller, the arithmetic does not.

// fit renders coeff × 10^exp as a Decimal, RAISING the exponent (coefficient
// divided by ten, half-up away from zero) until the coefficient fits an int64.
//
// This is the ONE place the rescale rule is written. Add, Mul, Abs and
// ToProtoScaled all end here, so a $184bn value rounds identically whichever
// operator produced it — the whole point of moving this out of compliance.
//
// It MUTATES coeff. Callers pass a big.Int they own.
//
// Three outcomes, and the third is the one that matters:
//
//   - the coefficient already fits at its natural exponent — returned as-is, so
//     every input that never wrapped keeps bit-identical behaviour;
//   - it does not fit — the exponent is raised until it does. This drops digits
//     that cannot matter at that magnitude and keeps the one thing a risk or
//     compliance rule needs: the MAGNITUDE;
//   - the exponent itself cannot be represented — ok=false, and the caller must
//     REFUSE. Never a wrapped number, and never a substituted zero: a zero
//     notional reads as "flat" and is dropped from every downstream check.
func fit(coeff *big.Int, exp int64) (*commonpb.Decimal, bool) {
	ten, five := big.NewInt(10), big.NewInt(5)
	rem := new(big.Int)
	for !coeff.IsInt64() || exp > math.MaxInt32 {
		if exp >= math.MaxInt32 {
			return nil, false // cannot raise the exponent any further
		}
		coeff.QuoRem(coeff, ten, rem)
		if rem.CmpAbs(five) >= 0 { // half-up, away from zero
			if rem.Sign() < 0 {
				coeff.Sub(coeff, big.NewInt(1))
			} else {
				coeff.Add(coeff, big.NewInt(1))
			}
		}
		exp++
	}
	return &commonpb.Decimal{Coefficient: coeff.Int64(), Exponent: int32(exp)}, true
}

// Mul returns a×b and whether the product is representable. nil is zero.
//
// The old bodies — compliance's before #86, risk/compute's before #216 —
// multiplied the coefficients as raw int64s, and a large-enough operand wrapped
// that product into a small, often NEGATIVE number. In compliance the rules saw
// a tiny position and ADMITTED an order worth billions. In the risk engine a
// -20% stress on a long book came back POSITIVE, which is a stress test that
// says the crash makes money.
//
// Both were reachable at plausible sizes for the same reason: dec.ToProtoScaled
// emits exponent -8 whenever that fits, so eight digits of int64 headroom are
// already spent before the multiply happens. At exponent -8 against a shock
// factor at -6, the product wraps above a position of $115,292.15.
//
// The product is therefore taken in math/big, exactly, and handed to fit.
func Mul(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) {
	if a == nil || b == nil {
		return &commonpb.Decimal{}, true
	}
	coeff := new(big.Int).Mul(big.NewInt(a.GetCoefficient()), big.NewInt(b.GetCoefficient()))
	// int64 accumulator: two int32 exponents can sum past int32 range.
	exp := int64(a.GetExponent()) + int64(b.GetExponent())
	if exp < math.MinInt32 {
		// Rescaling only ever RAISES the exponent; there is no way back from
		// here, and no realistic input reaches it. Refuse rather than guess.
		return nil, false
	}
	return fit(coeff, exp)
}

// Abs returns |d| and whether it is representable. nil is zero.
//
// It exists because negating an int64 coefficient is not total: -MinInt64 is
// MinInt64 again, so the naive `-c` returns a NEGATIVE absolute value. Folded
// into a gross exposure that subtracts a position from the book's size instead
// of adding it — the same sign flip Mul had, one operator over.
func Abs(d *commonpb.Decimal) (*commonpb.Decimal, bool) {
	if d == nil {
		return &commonpb.Decimal{}, true
	}
	c, e := d.GetCoefficient(), d.GetExponent()
	if c >= 0 {
		return &commonpb.Decimal{Coefficient: c, Exponent: e}, true
	}
	if c != math.MinInt64 {
		return &commonpb.Decimal{Coefficient: -c, Exponent: e}, true
	}
	// |MinInt64| is one past MaxInt64, so it has no int64 coefficient at this
	// exponent. fit raises the exponent by one digit, which is the same
	// magnitude-preserving move every other operator here makes.
	return fit(new(big.Int).Abs(big.NewInt(c)), int64(e))
}

// Add sums two Decimals exactly, or refuses.
//
// It aligns to the SMALLER exponent, which means scaling one operand up by
// 10^gap — and that is where it used to wrap: an int64 pow10 overflows at a gap
// of 19, so `Add(100e0, 1e-19)` returned 0.3875…, and at a gap of 25 the
// alignment went NEGATIVE, turning the sum of two positive quantities into a
// negative position. The sum itself could overflow independently even when both
// aligned operands fit.
//
// This is the same failure Mul had and is fixed the same way, because it is the
// same question: compute in math/big, rescale to a coarser exponent to keep the
// MAGNITUDE when the coefficient will not fit an int64, and refuse only when the
// exponent cannot move.
//
// THE ALIGNMENT EXPONENT IS CLAMPED, and that is load-bearing. Both exponents
// come off the wire and nothing upstream is guaranteed to constrain their range
// — the compliance gate runs BEFORE Accept validates the order, and the risk
// engine's gRPC surface had no domain check at all until #246. Aligning
// unconditionally to min(expA, expB) therefore lets a crafted input ask math/big
// for 10^4300000000, a multi-billion-digit bignum that hangs or OOMs the process
// long before the rescale loop can refuse anything. Exact-but-unbounded is worse
// than the int64 wrap it replaced: that at least returned in O(1).
//
// It is also unnecessary work. A gap that large MEANS the smaller operand sits
// billions of orders of magnitude below the larger; no int64 coefficient can
// hold a difference of even forty. So the alignment exponent is clamped to
// alignWindow digits below the LARGER exponent. Both scale factors are then
// bounded by 10^alignWindow and Exp is O(1) in the gap. Inside the window
// (every ordinary input) nothing changes: the clamp is inert and results stay
// bit-identical.
//
// Refusing large gaps instead would be the wrong answer: 10^9 + 10^-9 is
// perfectly representable, and a refusal on a legitimate order is a trading
// outage dressed as a control. We compute it; we just decline to compute digits
// that cannot survive the return type.
//
// ROUNDING: digits pushed out of the window are DROPPED (truncated toward zero),
// not carried as a rounding nudge. With alignWindow at 40 the larger operand
// aligns to at least 10^40, which fit must then rescale by ~22 exponent steps to
// reach an int64 — so everything the clamp discards lies below 10^-22 of the last
// digit the result can express. A nudge there could not move a representable
// digit; it would only add machinery pretending to a precision
// *commonpb.Decimal does not have.
func Add(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) {
	if a == nil {
		a = &commonpb.Decimal{}
	}
	if b == nil {
		b = &commonpb.Decimal{}
	}
	expA, expB := int64(a.GetExponent()), int64(b.GetExponent())
	// A zero coefficient has no magnitude, so its exponent must not drag the
	// alignment window: {0, e} + {c, f} is {c, f} for any e. Left in, a zero
	// operand carrying a wild exponent would clamp the real one away.
	if a.GetCoefficient() == 0 {
		expA = expB
	} else if b.GetCoefficient() == 0 {
		expB = expA
	}
	exp := AlignExponent(expA, expB)
	ten := big.NewInt(10)
	scale := func(d *commonpb.Decimal, dexp int64) *big.Int {
		c := big.NewInt(d.GetCoefficient())
		gap := dexp - exp // ≤ alignWindow by construction; negative only when clamped
		switch {
		case gap == 0:
			return c
		case gap > 0:
			return c.Mul(c, new(big.Int).Exp(ten, big.NewInt(gap), nil))
		default:
			// Clamped away: truncate toward zero. An int64 coefficient is
			// under 10^19, so anything shifted nineteen or more places right
			// IS zero — computing the divisor for a gap of billions would
			// reintroduce the very bignum this clamp exists to avoid.
			if -gap >= 19 {
				return big.NewInt(0)
			}
			// Quo truncates toward zero, which is the conservative reading of
			// a digit we are about to discard: it never grows the operand's
			// magnitude. Div (floor) would be observationally identical here —
			// the two differ only in the last unit of the truncated operand,
			// and by construction that operand sits at least twenty-one orders
			// of magnitude below the last digit the result can express. The
			// choice is stated for intent, not because a rule can see it.
			return c.Quo(c, new(big.Int).Exp(ten, big.NewInt(-gap), nil))
		}
	}
	return fit(new(big.Int).Add(scale(a, expA), scale(b, expB)), exp)
}

// alignWindow is how many decimal digits below the larger operand's exponent
// Add bothers to align. An int64 coefficient carries ~19 significant digits; 40
// is comfortably past double that, so every digit inside the window that could
// ever reach the result is kept, with room to spare.
const alignWindow = 40

// AlignExponent returns the exponent Add aligns both operands to, given their
// (zero-coefficient-adjusted) exponents. It is the smaller of the two, but never
// lower than the larger minus alignWindow — see "THE ALIGNMENT EXPONENT IS
// CLAMPED" on Add for why an unclamped min(expA, expB) is a DoS and why
// alignWindow is the right bound.
//
// Exported so the callers that depend on the clamp can pin the invariant
// directly — a Decimal-level assertion cannot observe the clamp turning on or
// off (fit re-rounds the difference away), so the only place it is testable is
// this function's own return value.
func AlignExponent(expA, expB int64) int64 {
	lo, hi := expA, expB
	if hi < lo {
		lo, hi = hi, lo
	}
	exp := lo
	if hi-alignWindow > exp {
		exp = hi - alignWindow
	}
	return exp
}
