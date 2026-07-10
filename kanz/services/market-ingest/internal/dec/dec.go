// Package dec is market-ingest's exact-decimal helper over common.v1.Decimal.
// L2 book prices and sizes must never touch float64 — the platform rule
// (common.v1.Decimal exists precisely for this); an order-book-imbalance or
// arbitrage decision computed on a float is a silent capital risk. big.Rat gives
// exact compare/add/sub; ToProto rounds a rational back to a fixed-scale Decimal
// half-up, the only place rounding is introduced.
//
// This mirrors services/oms/internal/dec and services/webhook-ingest/internal/
// dec on purpose: identical Decimal↔big.Rat semantics across every hop are what
// keep the zero-float guarantee intact. The consumers should collapse into a
// shared pkg/dec at the Milestone-5 zero-float audit; kept service-local now to
// match the established pattern rather than refactoring delivered code.
package dec

import (
	"errors"
	"math/big"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

const scale = 8

// ParseRat parses a decimal string ("0.1", "50000") into an exact rational.
func ParseRat(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("dec: not a decimal number: " + s)
	}
	return r, nil
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

// IsZero reports whether d == 0.
func IsZero(d *commonpb.Decimal) bool { return FromProto(d).Sign() == 0 }
