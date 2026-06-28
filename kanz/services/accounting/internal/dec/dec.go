// Package dec is the accounting book's exact-decimal helper over
// common.v1.Decimal. Book-of-record math (cash balances, position cost, NAV)
// must never touch float64 — the platform rule (common.v1.Decimal exists
// precisely for this; EVT-14). big.Rat gives exact add/sub/mul; ToProto rounds a
// rational back to a fixed-scale Decimal half-up, the only place rounding is
// introduced.
//
// It mirrors the OMS dec helper; the Go internal-package rule forbids importing
// that one across the service boundary, so the book carries its own.
package dec

import (
	"math/big"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// scale is the number of fractional digits ToProto emits — eight covers price
// and quantity precision across the modelled asset classes without overflowing
// the Decimal int64 coefficient for realistic notionals.
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

// Rat is a convenience constructor for an exact rational from a string like
// "1.25" or "-100". It panics on a malformed literal — callers pass constants.
func Rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("dec: bad rational literal " + s)
	}
	return r
}
