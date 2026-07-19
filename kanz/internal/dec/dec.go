// Package dec is the platform's exact-decimal helper over common.v1.Decimal —
// the single implementation of the zero-float rule.
//
// Money, prices, quantities and L2 depth must NEVER touch float64: an order size,
// an order-book imbalance, or a NAV computed on a float is a silent capital risk,
// which is exactly why common.v1.Decimal exists. big.Rat gives exact
// compare/add/sub/mul/quo; ToProto rounds a rational back to a fixed-scale
// Decimal half-up, and is the ONLY place rounding is introduced.
//
// This package was consolidated at Milestone 5 from five byte-identical
// service-local copies (oms, webhook-ingest, market-ingest, accounting, tv-sync),
// each exporting a different subset of the same functions. Identical
// Decimal↔big.Rat semantics on every hop are what keep the zero-float guarantee
// intact end to end — five copies were five chances for them to drift apart.
package dec

import (
	"errors"
	"math"
	"math/big"
	"strings"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// scale is the fixed decimal scale ToProto rounds to.
const scale = 8

// ParseRat parses a decimal string ("0.1", "50000") into an exact rational.
func ParseRat(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("dec: not a decimal number: " + s)
	}
	return r, nil
}

// Rat parses a decimal LITERAL and panics if it is malformed. For constants in
// code and tests only — never for external input, which must use ParseRat.
func Rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("dec: bad rational literal " + s)
	}
	return r
}

// FromProto converts a common.v1.Decimal to an exact rational. A nil Decimal is
// zero.
func FromProto(d *commonpb.Decimal) *big.Rat {
	r := new(big.Rat)
	if d == nil {
		return r
	}
	coeff := big.NewInt(d.GetCoefficient())
	exp := d.GetExponent()
	if exp >= 0 {
		mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
		return r.SetInt(new(big.Int).Mul(coeff, mul))
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exp)), nil)
	return r.SetFrac(coeff, den)
}

// maxSafeExponent bounds the exponent FromProtoChecked will accept.
//
// It is a SAFETY limit, not a statement about what money means on this
// platform: real financial values keep |exponent| well under 30 (the
// smallest crypto prices sit near 1e-12, the largest plausible notionals
// near 1e13), so nothing legitimate is within thirty orders of magnitude of
// this bound and it cannot refuse a real value.
//
// The number mirrors internal/compliance's maxDecimalExponent (gate.go),
// deliberately: it is the same question — how far can 10^abs(exponent) be
// materialised before the computation itself becomes the incident — asked
// at a different boundary. dec and compliance must not import each other,
// so the value is duplicated here as a number rather than shared as a
// constant; keeping it numerically identical is what keeps the platform
// coherent. FromProto({Coefficient:1, Exponent:2000000000}) does not return
// within seconds; {Coefficient:1, Exponent:64} is instant.
const maxSafeExponent = 64

// FromProtoChecked is FromProto with a bounded domain: it refuses an
// out-of-domain exponent instead of hanging computing 10^abs(exponent).
//
// FromProto's signature and behaviour are UNCHANGED — it has many callers
// this package does not audit, and the same reasoning that keeps ToProto
// intact applies here. FromProtoChecked is the variant any UNTRUSTED
// input — wire data that has not passed through a validating gate, such as
// a MarketDataEvent price off the market.> subject — must use instead.
//
// ok == false means the exponent's magnitude exceeds maxSafeExponent; the
// caller must treat the Decimal as unusable, exactly as it would a
// malformed message, and must NEVER substitute zero. A zero price is not a
// safe fallback anywhere on this platform: it can be silently treated as
// "flat" and dropped from downstream checks (see mark.Handle's callers).
func FromProtoChecked(d *commonpb.Decimal) (*big.Rat, bool) {
	if d == nil {
		return new(big.Rat), true
	}
	exp := d.GetExponent()
	if exp > maxSafeExponent || exp < -maxSafeExponent {
		return nil, false
	}
	return FromProto(d), true
}

// scaledCoefficient rounds a rational to the fixed scale (half-up) and returns
// the resulting coefficient as a big.Int. This is the ONE place the
// scaling/rounding arithmetic is written; ToProto and ToProtoScaled both call
// it so their behaviour cannot drift apart.
func scaledCoefficient(r *big.Rat) *big.Int {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaledNum := new(big.Int).Mul(r.Num(), pow)
	q, rem := new(big.Int).QuoRem(scaledNum, r.Denom(), new(big.Int))
	twice := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2))
	if twice.Cmp(new(big.Int).Abs(r.Denom())) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// ToProto rounds a rational to a fixed-scale common.v1.Decimal (half-up). Its
// signature and behaviour are UNCHANGED and stay that way: this keeps
// wrapping (via big.Int.Int64(), per math/big's own documented behaviour) for
// values whose scaled coefficient does not fit an int64, which is what its
// ~30 remaining non-capital callers (reporting, analytics) already depend on.
// Callers on a capital path — anywhere a wrapped, fabricated coefficient
// would be acted on — must use ToProtoScaled instead.
func ToProto(r *big.Rat) *commonpb.Decimal {
	q := scaledCoefficient(r)
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: -scale}
}

// ToProtoScaled converts an exact rational to a Decimal, preserving MAGNITUDE.
//
// It emits at the fixed scale when the coefficient fits an int64; otherwise it
// raises the exponent (half-up, away from zero) until it does, and refuses only
// when the exponent itself cannot move. This is the same shape as
// internal/compliance's mulDecimal, deliberately: it is the same question asked
// of a different operator.
//
// Use this on any capital path. ToProto wraps at roughly $92bn at scale -8, and
// a wrapped coefficient is a fabricated number the system will then act on. The
// alternative — refusing a large-but-real value — turns it into a failed
// operation, which on an order path is a refused trade. You do not need eight
// decimal places on $100bn; you do need the magnitude to be right.
func ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool) {
	if r == nil {
		return &commonpb.Decimal{}, true
	}
	q := scaledCoefficient(r)
	exp := int64(-scale)
	ten, five := big.NewInt(10), big.NewInt(5)
	rem := new(big.Int)
	for !q.IsInt64() {
		if exp >= math.MaxInt32 {
			return nil, false // cannot raise the exponent any further
		}
		q.QuoRem(q, ten, rem)
		if rem.CmpAbs(five) >= 0 { // half-up, away from zero
			if rem.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
		exp++
	}
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: int32(exp)}, true
}

// Str renders a rational as a trimmed plain-decimal string.
func Str(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	s := r.FloatString(scale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-0" {
		s = "0"
	}
	return s
}

// IsPositive reports whether d > 0.
func IsPositive(d *commonpb.Decimal) bool { return FromProto(d).Sign() > 0 }

// IsZero reports whether d == 0.
func IsZero(d *commonpb.Decimal) bool { return FromProto(d).Sign() == 0 }

// Cmp compares two Decimals exactly: -1 if a < b, 0 if equal, +1 if a > b.
func Cmp(a, b *commonpb.Decimal) int { return FromProto(a).Cmp(FromProto(b)) }
