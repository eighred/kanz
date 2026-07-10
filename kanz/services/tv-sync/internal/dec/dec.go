// Package dec is tv-sync's exact-decimal helper over common.v1.Decimal. Position
// folding, average-cost, and realized P&L must never touch float64 — the
// platform zero-float rule. big.Rat gives exact arithmetic; Str renders a value
// for the JSON read surface without introducing a binary-float artifact.
//
// Mirrors services/oms/internal/dec and services/webhook-ingest/internal/dec;
// the several copies collapse into a shared pkg/dec at the Milestone-5
// zero-float audit (kept service-local now to avoid refactoring delivered code
// mid-build).
package dec

import (
	"math/big"
	"strings"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

const scale = 8

// FromProto converts a common.v1.Decimal to an exact rational (nil ⇒ zero).
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

// ToProto rounds a rational to a fixed-scale common.v1.Decimal (half-up).
func ToProto(r *big.Rat) *commonpb.Decimal {
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
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: -scale}
}

// Str renders a rational as a trimmed decimal string (up to 8 dp) for the JSON
// read surface — "0.5", "50000", "-1.25". Exact to the scale; trailing zeros
// and a dangling point are trimmed.
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
