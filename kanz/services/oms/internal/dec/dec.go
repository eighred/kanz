// Package dec is the OMS's exact-decimal helper over common.v1.Decimal. Order
// math (filled-quantity sums, average prices, realized P&L) must never touch
// float64 — the platform rule (common.v1.Decimal exists precisely for this).
// big.Rat gives exact add/sub/mul; ToProto rounds a rational back to a
// fixed-scale Decimal half-up, the only place rounding is introduced.
package dec

import (
	"math/big"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// scale is the number of fractional digits ToProto emits. Eight covers price
// and quantity precision across the asset classes the reference data models
// without overflowing the Decimal int64 coefficient for realistic notionals.
const scale = 8

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

// ToProto rounds a rational to a fixed-scale common.v1.Decimal (half-up).
func ToProto(r *big.Rat) *commonpb.Decimal {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	// scaled = round(r * 10^scale) = round(num*pow / den)
	scaledNum := new(big.Int).Mul(r.Num(), pow)
	q, rem := new(big.Int).QuoRem(scaledNum, r.Denom(), new(big.Int))
	// Half-up rounding on the absolute remainder.
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

// IsPositive reports whether d > 0.
func IsPositive(d *commonpb.Decimal) bool { return FromProto(d).Sign() > 0 }

// IsZero reports whether d == 0.
func IsZero(d *commonpb.Decimal) bool { return FromProto(d).Sign() == 0 }

// Cmp compares a and b: -1, 0, or +1.
func Cmp(a, b *commonpb.Decimal) int { return FromProto(a).Cmp(FromProto(b)) }
