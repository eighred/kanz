package termsource

import (
	"context"
	"errors"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing"
)

// Provider resolves contract terms from the store, satisfying
// compute.TermsProvider — the seam the Greeks layer and the revaluation path
// consume, and which until now had NO production implementation (#345).
//
// # Why the missing case is a counter and not an error
//
// compute.TermsProvider returns (OptionSpec, bool) with no error channel, and
// greeks.go states the contract it built that on: "isOption=false when the
// position is not an option OR its pricing inputs are unavailable (the caller
// falls back to the linear treatment)". Both collapse to false, deliberately.
//
// That is fine for a share and dangerous for a derivative whose terms were never
// LOADED — the same instrument the estate believes it is pricing as an option
// gets priced as if it were stock, quietly. Widening the seam would touch every
// consumer and is a change worth taking on its own evidence, not smuggled in
// with the first implementation.
//
// So the resolution is made OBSERVABLE instead: onMissing fires for every
// instrument the store has no terms for. It cannot say which of those are really
// options — that needs reference.v1.InstrumentReference.asset_class, which
// nothing stores yet — but a count that climbs while the book holds derivatives
// is the signal that terms are not being loaded, and it exists rather than the
// silence that exists today.
type Provider struct {
	store     *terms.Postgres
	onMissing func(instrumentID string)
}

// ProviderOption customizes a Provider.
type ProviderOption func(*Provider)

// WithMissingTermsObserver sets the hook invoked when the store holds no terms
// for an instrument — the "priced linearly because we had nothing" signal. Nil
// (the default) means the case is unobserved, which is the state before this
// type existed; wire it at the composition root.
func WithMissingTermsObserver(fn func(instrumentID string)) ProviderOption {
	return func(p *Provider) { p.onMissing = fn }
}

// NewProvider returns a compute.TermsProvider over the store.
func NewProvider(store *terms.Postgres, opts ...ProviderOption) *Provider {
	p := &Provider{store: store}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Compile-time assertion that this satisfies the seam it exists for. Without it
// a signature drift in compute.TermsProvider would be discovered at the
// composition root rather than here.
var _ compute.TermsProvider = (*Provider)(nil)

// OptionTerms resolves an instrument's option terms as of a point in time.
// ok=false means "do not price this as an option" — see the type comment for
// what that does and does not distinguish.
func (p *Provider) OptionTerms(ctx context.Context, instrumentID string, asOf time.Time) (compute.OptionSpec, bool) {
	if p == nil || p.store == nil {
		return compute.OptionSpec{}, false
	}
	rec, err := p.store.LatestAsOf(ctx, instrumentID, asOf)
	if err != nil {
		if errors.Is(err, terms.ErrNoTerms) && p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		// A STORE ERROR ALSO RETURNS false, and that is forced rather than
		// chosen: the seam has no error channel. It is not silent, though — a
		// database that cannot be read fails the readiness Ping long before it
		// fails here, so the loud signal exists upstream of this call.
		return compute.OptionSpec{}, false
	}
	opt := rec.Terms.GetOption()
	if opt == nil {
		// A swap or a future. Genuinely "not an option": the linear fallback is
		// the correct answer and this is NOT the missing-terms case, so the
		// observer deliberately does not fire.
		return compute.OptionSpec{}, false
	}
	spec, ok := toSpec(opt)
	if !ok {
		// Terms that exist but cannot be used — a non-positive strike or
		// multiplier, an unspecified type. Reported as missing, because the
		// consequence is identical: this option is about to be priced linearly.
		if p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		return compute.OptionSpec{}, false
	}
	return spec, true
}

// toSpec converts reference.v1.OptionTerms to the float working shape the
// pricing layer uses.
//
// THE DECIMAL BOUNDARY IS HERE, AND IT IS THE ONLY PLACE IT SHOULD BE. The wire
// and the store carry common.v1.Decimal because money and quantities are exact;
// pricing is numerical (Black-Scholes, SVI), so compute.OptionSpec is float64 and
// its own comment calls it "the float working shape behind
// reference.v1.OptionTerms". Converting anywhere else would put a float on a
// path that is supposed to be exact.
//
// A term that cannot be represented is refused rather than defaulted: a zero
// strike prices as a forward and a zero multiplier makes every Greek zero, and
// both would look like an option that simply has no risk.
func toSpec(o *referencepb.OptionTerms) (compute.OptionSpec, bool) {
	strike, ok := decimalToFloat(o.GetStrike())
	if !ok || strike <= 0 {
		return compute.OptionSpec{}, false
	}
	mult, ok := decimalToFloat(o.GetContractMultiplier())
	if !ok || mult <= 0 {
		return compute.OptionSpec{}, false
	}
	if o.GetExpiry() == nil {
		return compute.OptionSpec{}, false
	}
	var typ pricing.OptionType
	switch o.GetOptionType() {
	case referencepb.OptionType_OPTION_TYPE_CALL:
		typ = pricing.Call
	case referencepb.OptionType_OPTION_TYPE_PUT:
		typ = pricing.Put
	default:
		// OPTION_TYPE_UNSPECIFIED. Guessing call would misprice every put by the
		// whole put-call parity gap, in the direction that understates risk on a
		// short book.
		return compute.OptionSpec{}, false
	}
	exercise := pricing.European
	if o.GetExerciseStyle() == referencepb.ExerciseStyle_EXERCISE_STYLE_AMERICAN {
		exercise = pricing.American
	}
	return compute.OptionSpec{
		UnderlyingID: o.GetUnderlyingId(),
		Strike:       strike,
		Expiry:       o.GetExpiry().AsTime(),
		Type:         typ,
		Exercise:     exercise,
		Multiplier:   mult,
	}, true
}

// decimalToFloat converts an exact decimal to the pricing layer's float. ok is
// false for a nil decimal, so an absent term is refused rather than read as 0.
func decimalToFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	r := new(big.Rat).SetInt64(d.GetCoefficient())
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs32(d.GetExponent()))), nil))
	if d.GetExponent() < 0 {
		r.Quo(r, scale)
	} else {
		r.Mul(r, scale)
	}
	f, _ := r.Float64()
	return f, true
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// Compile-time assertion for the fixed-income seam, alongside the option one
// above. RegisterFIRisk takes this interface and had no implementation at all
// until #509, which is why DV01, Duration, Convexity and SpreadDuration were
// implemented and unreachable.
var _ compute.BondTermsProvider = (*Provider)(nil)

// BondTerms resolves an instrument's bond terms as of a point in time.
// ok=false means "this is not a bond, or its terms cannot be used" — the same
// collapsed signal OptionTerms carries, for the same reason: the seam has no
// error channel.
func (p *Provider) BondTerms(ctx context.Context, instrumentID string, asOf time.Time) (compute.BondSpec, bool) {
	if p == nil || p.store == nil {
		return compute.BondSpec{}, false
	}
	rec, err := p.store.LatestAsOf(ctx, instrumentID, asOf)
	if err != nil {
		if errors.Is(err, terms.ErrNoTerms) && p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		return compute.BondSpec{}, false
	}
	bond := rec.Terms.GetBond()
	if bond == nil {
		// An option, a swap or a future. Genuinely not a bond — the FI measures
		// skip it, which is correct — so the missing-terms observer does not fire.
		return compute.BondSpec{}, false
	}
	spec, ok := toBondSpec(bond)
	if !ok {
		// Terms that exist and cannot be used. Reported as missing because the
		// consequence is the same: this bond is about to be excluded from DV01
		// and from every duration average, silently reducing the book's measured
		// rate risk.
		if p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		return compute.BondSpec{}, false
	}
	return spec, true
}

// toBondSpec converts reference.v1.BondTerms to the float working shape the bond
// library uses.
func toBondSpec(b *referencepb.BondTerms) (compute.BondSpec, bool) {
	face, ok := decimalToFloat(b.GetFaceValue())
	if !ok || face <= 0 {
		return compute.BondSpec{}, false
	}
	// A ZERO COUPON IS VALID and is not a missing one — BondTerms says so: "0
	// denotes a zero-coupon bond". So this checks convertibility and sign, not
	// non-zero, and a negative coupon is refused because it is not a bond this
	// library models.
	coupon, ok := decimalToFloat(b.GetCouponRate())
	if !ok || coupon < 0 {
		return compute.BondSpec{}, false
	}
	if b.GetIssueDate() == nil || b.GetMaturityDate() == nil {
		return compute.BondSpec{}, false
	}
	issue, maturity := b.GetIssueDate().AsTime(), b.GetMaturityDate().AsTime()
	if !maturity.After(issue) {
		// A bond maturing at or before issue has no cashflow schedule; the
		// pricer would loop over an empty or inverted range and return a price
		// with no error.
		return compute.BondSpec{}, false
	}
	if b.GetCurrencyCode() == "" {
		// The currency is the KEY the discount curve is looked up by. An empty
		// one resolves no curve, so the bond drops out of every FI measure — and
		// it would do so silently, which is why it is refused here where the
		// observer fires.
		return compute.BondSpec{}, false
	}
	dc, ok := toDayCount(b.GetDayCount())
	if !ok {
		return compute.BondSpec{}, false
	}
	freq, ok := toFrequency(b.GetCouponFrequency(), coupon)
	if !ok {
		return compute.BondSpec{}, false
	}
	return compute.BondSpec{
		Face: face, CouponRate: coupon, Frequency: freq,
		Issue: issue, Maturity: maturity, DayCount: dc,
		Currency: b.GetCurrencyCode(), IssuerID: b.GetIssuerId(),
	}, true
}

// toDayCount maps the wire convention to the pricing one.
//
// EXPLICIT, NOT A CAST, and the numbers are why. The proto and the Go iota
// disagree on ordering — DAY_COUNT_CONVENTION_ACT_ACT is 4 while
// pricing.ActualActual is 0 — so pricing.DayCount(wireValue) would send ACT/ACT
// out of range and, far worse, map UNSPECIFIED(0) onto ActualActual, the
// government-bond basis. An unset convention would become a valid, plausible one
// and every accrual computed from it would be wrong by a few days' interest with
// nothing to indicate it.
func toDayCount(d referencepb.DayCountConvention) (pricing.DayCount, bool) {
	switch d {
	case referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_ACT:
		return pricing.ActualActual, true
	case referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_365F:
		return pricing.Actual365Fixed, true
	case referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_360:
		return pricing.Actual360, true
	case referencepb.DayCountConvention_DAY_COUNT_CONVENTION_THIRTY_360:
		return pricing.Thirty360, true
	default:
		return 0, false
	}
}

// toFrequency maps the payment frequency to coupons per year.
//
// A ZERO-COUPON BOND NEEDS NO FREQUENCY, which BondTerms states — the field is
// "ignored (no coupons) when coupon_rate is 0" — so an unspecified frequency is
// accepted there and refused everywhere else. Defaulting a coupon bond's
// frequency to annual would halve a semiannual bond's coupon count and understate
// its duration; refusing makes the operator say which it is.
func toFrequency(f referencepb.PaymentFrequency, couponRate float64) (int, bool) {
	if couponRate == 0 {
		return 0, true
	}
	switch f {
	case referencepb.PaymentFrequency_PAYMENT_FREQUENCY_ANNUAL:
		return 1, true
	case referencepb.PaymentFrequency_PAYMENT_FREQUENCY_SEMIANNUAL:
		return 2, true
	case referencepb.PaymentFrequency_PAYMENT_FREQUENCY_QUARTERLY:
		return 4, true
	case referencepb.PaymentFrequency_PAYMENT_FREQUENCY_MONTHLY:
		return 12, true
	default:
		return 0, false
	}
}
