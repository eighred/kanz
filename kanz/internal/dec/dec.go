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
	"fmt"
	"math/big"
	"strings"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// scale is the fixed decimal scale ToProto rounds to.
const scale = 8

// Scale is the number of decimal places Str renders at, and therefore the
// smallest magnitude this platform can express as a decimal string.
//
// EXPORTED SO A CALLER CAN REFUSE RATHER THAN ROUND. A value below 10^-Scale
// renders as "0" — which is not a smaller number, it is a DIFFERENT CLAIM, and
// on a signed regulatory filing it is the claim "we measured zero" (#672).
// internal/filing reads this to say so in its refusal.
const Scale = scale

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
// at a different boundary. It is duplicated as a number rather than shared
// as a constant, and test/arch/decimal_domain_test.go is what keeps the two
// literals equal; a comment naming a sibling is not a check.
//
// CORRECTED (#216): this used to say "dec and compliance must not import
// each other", which was not true in the direction that mattered —
// internal/compliance has imported this package (as decutil) for some time,
// so the constant COULD be shared today. Only the reverse edge is
// forbidden, and it is forbidden by the cycle, not by a rule. Unifying the
// two is a separate change: the arch guard reads both literals out of the
// source, so retiring one means rewriting the guard at the same time.
//
// FromProto({Coefficient:1, Exponent:2000000000}) does not return within
// seconds; {Coefficient:1, Exponent:64} is instant.
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
	if !InDomain(d) {
		return nil, false
	}
	return FromProto(d), true
}

// InDomain reports whether a Decimal can be converted without materialising an
// exponent large enough to be the incident itself. A nil Decimal is in-domain —
// absent is not out-of-range, and whatever owns "this field is required" owns
// that case.
//
// This is the predicate FromProtoChecked answers with; it is exported so a caller
// that must refuse a whole MESSAGE before decoding any of it can ask the question
// without converting field by field.
func InDomain(d *commonpb.Decimal) bool {
	if d == nil {
		return true
	}
	exp := d.GetExponent()
	return exp >= -maxSafeExponent && exp <= maxSafeExponent
}

// ErrCurrencyMismatch is what MoneyIn refuses with. Callers that must keep
// consuming past one bad message (a bus handler) match on it with errors.Is.
var ErrCurrencyMismatch = errors.New("dec: money is not in the expected currency")

// MoneyIn returns m's amount as an exact rational, but ONLY if m is denominated
// in currency; otherwise it refuses with ErrCurrencyMismatch.
//
// THIS IS THE GUARD ON NETTING A Money AGAINST A BALANCE. common.v1.Money carries
// a currency_code and the arithmetic does not: subtracting a BTC fee from a USD
// cash leg compiles, runs, and produces a number that is economically nonsense —
// it debits cash that never moved and leaves the asset that DID move unrecorded.
// Books are append-only, so such an entry is permanent and compounds per event;
// refusing at the seam stops one message instead of corrupting NAV forever.
//
// A zero (or nil) Money is zero in every currency and has no cash effect, so it
// is accepted whatever its currency_code says — a fill with no fee must keep
// working. A NON-ZERO Money with an EMPTY currency_code is refused: "nobody
// stamped this" and "stamped with the right currency" must not look the same.
func MoneyIn(m *commonpb.Money, currency string) (*big.Rat, error) {
	amt := FromProto(m.GetAmount())
	if amt.Sign() == 0 {
		return amt, nil
	}
	if got := m.GetCurrencyCode(); got != currency {
		return nil, fmt.Errorf("%w: %s in %q cannot net against a %q balance",
			ErrCurrencyMismatch, Str(amt), got, currency)
	}
	return amt, nil
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
// when the exponent itself cannot move — the shared `fit` rule in arith.go, the
// same one Add, Mul and Abs end on. It has to be the same one: a value rounded
// differently depending on which operator produced it is a reconciliation break
// nobody would think to look for.
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
	return fit(scaledCoefficient(r), -scale)
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

// THE PREDICATES BELOW NEVER MATERIALISE 10^abs(exponent).
//
// They used to answer via FromProto, which meant every one of them inherited its
// hang: dec.IsPositive(cmd.GetQuantity()) on an order carrying
// {Coefficient:1, Exponent:2000000000} did not return, and Decimal.exponent is an
// unvalidated wire field. That reached the OMS order aggregate, the venue
// adapters' limit-price checks and the execution router — paths with no domain
// gate in front of them (#95).
//
// They are answered arithmetically instead. This is not an approximation and not
// a bounded domain: the results are IDENTICAL to the expanding versions for every
// input, including the ones that used to hang, because 10^exponent is strictly
// positive and therefore cannot change a sign or a zero. A caller needing the
// VALUE still has to use FromProtoChecked and handle the refusal — only the
// questions answerable without the value are answered here.

// sign reports the sign of a Decimal: -1, 0 or +1. A nil Decimal is zero.
//
// sign(coefficient × 10^exponent) == sign(coefficient), because 10^exponent is
// positive for every exponent. The exponent is therefore not read at all, which
// is what makes this total.
func sign(d *commonpb.Decimal) int {
	switch c := d.GetCoefficient(); {
	case c > 0:
		return 1
	case c < 0:
		return -1
	default:
		return 0
	}
}

// IsPositive reports whether d > 0.
func IsPositive(d *commonpb.Decimal) bool { return sign(d) > 0 }

// IsZero reports whether d == 0.
func IsZero(d *commonpb.Decimal) bool { return sign(d) == 0 }

// Cmp compares two Decimals exactly: -1 if a < b, 0 if equal, +1 if a > b.
func Cmp(a, b *commonpb.Decimal) int {
	sa, sb := sign(a), sign(b)
	if sa != sb {
		if sa < sb {
			return -1
		}
		return 1
	}
	if sa == 0 {
		return 0 // both zero, whatever their exponents
	}
	if sa < 0 {
		return -cmpMagnitude(a, b) // more negative is smaller
	}
	return cmpMagnitude(a, b)
}

// cmpMagnitude compares |a| against |b|. Both must be non-zero.
//
// It compares DECIMAL ORDER first — digits(coefficient) + exponent, the position
// of the leading digit — because a value with more digits before the point is
// larger outright: |v| lies in [10^(order-1), 10^order), so a strictly greater
// order is a strictly greater magnitude and no alignment is needed.
//
// Alignment is reached only when the orders TIE, and that is what bounds the
// arithmetic: equal orders mean the exponent gap equals the digit-count gap,
// which is at most 18 for an int64 coefficient. The largest power this can ever
// raise is 10^18 — instant — no matter how extreme the exponents themselves are.
func cmpMagnitude(a, b *commonpb.Decimal) int {
	ca := new(big.Int).Abs(big.NewInt(a.GetCoefficient()))
	cb := new(big.Int).Abs(big.NewInt(b.GetCoefficient()))
	ea, eb := int64(a.GetExponent()), int64(b.GetExponent())

	orderA := int64(len(ca.String())) + ea
	orderB := int64(len(cb.String())) + eb
	if orderA != orderB {
		if orderA < orderB {
			return -1
		}
		return 1
	}

	switch {
	case ea > eb:
		ca.Mul(ca, new(big.Int).Exp(big.NewInt(10), big.NewInt(ea-eb), nil))
	case eb > ea:
		cb.Mul(cb, new(big.Int).Exp(big.NewInt(10), big.NewInt(eb-ea), nil))
	}
	return ca.Cmp(cb)
}
