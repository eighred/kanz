// Package dec is webhook-ingest's exact-decimal helper over common.v1.Decimal.
// Size translation (percent-of-NAV, quote-notional → absolute quantity) and
// per-venue allocation scaling must never touch float64 — the platform rule
// (common.v1.Decimal exists precisely for this). big.Rat gives exact
// add/sub/mul/div; ToProto rounds a rational back to a fixed-scale Decimal
// half-up, the only place rounding is introduced.
//
// This mirrors services/oms/internal/dec byte-for-byte on purpose: identical
// Decimal↔big.Rat semantics across the signal→order boundary are what keep the
// zero-float guarantee intact end to end. The three-plus consumers
// (oms/dec, compliance, here) should collapse into a shared pkg/dec at the
// Milestone-5 zero-float audit; kept service-local now to avoid refactoring
// delivered code mid-pipeline-build.
package dec

import (
	"errors"
	"math/big"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// ParseRat parses a decimal string ("0.1", "5000", "5") into an exact rational.
// Webhook payloads carry sizes/prices as STRINGS, never JSON numbers, so no
// float ever enters the pipeline. An empty or malformed string is an error.
func ParseRat(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("dec: not a decimal number: " + s)
	}
	return r, nil
}

// scale is the number of fractional digits ToProto emits — matches oms/dec so a
// quantity resolved here and one summed there round identically.
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

// IsPositive reports whether d > 0.
func IsPositive(d *commonpb.Decimal) bool { return FromProto(d).Sign() > 0 }
