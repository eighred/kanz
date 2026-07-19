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

// scaledCoefficient rounds a rational to the fixed scale (half-up) and returns
// the resulting coefficient as a big.Int. This is the ONE place the
// scaling/rounding arithmetic is written; ToProto and ToProtoExact both call
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
// signature and behaviour are unchanged by ToProtoExact's addition: callers
// that never see coefficients near the int64 boundary are unaffected, and
// this keeps wrapping (via big.Int.Int64(), per math/big's own documented
// behaviour) for values that don't fit — callers needing a representability
// check must use ToProtoExact instead.
func ToProto(r *big.Rat) *commonpb.Decimal {
	q := scaledCoefficient(r)
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: -scale}
}

// ToProtoExact is ToProto's representability-checked counterpart: it performs
// the identical scaling and half-up rounding, but reports ok == false instead
// of silently wrapping when the rounded coefficient does not fit in an int64.
// Callers on a capital path — anywhere an out-of-range value must REFUSE
// rather than be valued at a wrapped, fabricated coefficient — should call
// this instead of ToProto. When ok is false, the returned *commonpb.Decimal
// is nil.
func ToProtoExact(r *big.Rat) (d *commonpb.Decimal, ok bool) {
	q := scaledCoefficient(r)
	if !q.IsInt64() {
		return nil, false
	}
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: -scale}, true
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
