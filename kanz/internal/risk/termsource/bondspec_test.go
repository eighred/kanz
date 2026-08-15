package termsource

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/risk/pricing"
)

// THE BOND TERMS CONVERSION (#509).
//
// UNGATED, unlike the store tests in provider_test.go. The conversion is a pure
// function and it is where the correctness lives — the enum mappings below are
// the part that produces a plausible wrong answer rather than an error — so it is
// tested without a database, where it actually runs on every machine instead of
// skipping on the ones without TEST_POSTGRES_URL.

var bondIssue = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

func bdec(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

// aBond is a five-year 5% semiannual, ACT/ACT, USD.
func aBond() *referencepb.BondTerms {
	return &referencepb.BondTerms{
		FaceValue:       bdec(1000, 0),
		CouponRate:      bdec(5, -2), // 0.05
		CouponFrequency: referencepb.PaymentFrequency_PAYMENT_FREQUENCY_SEMIANNUAL,
		IssueDate:       timestamppb.New(bondIssue),
		MaturityDate:    timestamppb.New(bondIssue.AddDate(5, 0, 0)),
		DayCount:        referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_ACT,
		CurrencyCode:    "USD",
		IssuerId:        "US-TREASURY",
	}
}

// A COMPLETE BOND MAPS COMPLETELY.
func TestToBondSpec_MapsEveryField(t *testing.T) {
	spec, ok := toBondSpec(aBond())
	if !ok {
		t.Fatal("a well-formed bond did not convert")
	}
	if spec.Face != 1000 {
		t.Errorf("face = %v, want 1000", spec.Face)
	}
	if spec.CouponRate != 0.05 {
		t.Errorf("coupon = %v, want 0.05 — a conversion reading only the coefficient would "+
			"give 5, a 500%% coupon", spec.CouponRate)
	}
	if spec.Frequency != 2 {
		t.Errorf("frequency = %v, want 2", spec.Frequency)
	}
	if spec.DayCount != pricing.ActualActual {
		t.Errorf("day count = %v, want ActualActual", spec.DayCount)
	}
	if spec.Currency != "USD" || spec.IssuerID != "US-TREASURY" {
		t.Errorf("currency/issuer = %q/%q", spec.Currency, spec.IssuerID)
	}
	if !spec.Maturity.After(spec.Issue) {
		t.Errorf("issue %v is not before maturity %v", spec.Issue, spec.Maturity)
	}
}

// EVERY DAY COUNT MAPS TO THE RIGHT CONVENTION, AND THE MAP IS NOT A CAST.
//
// This is the case that matters most in the file. The proto and the Go iota
// disagree on ordering — ACT_ACT is 4 on the wire and pricing.ActualActual is 0 —
// so pricing.DayCount(wireValue) would put ACT/ACT out of range entirely and map
// UNSPECIFIED(0) onto ActualActual. An unset convention would silently become the
// government-bond basis, and every accrual computed from it would be wrong by a
// few days' interest with nothing anywhere to indicate it.
func TestToDayCount_MapsExplicitlyRatherThanCasting(t *testing.T) {
	cases := []struct {
		wire referencepb.DayCountConvention
		want pricing.DayCount
	}{
		{referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_ACT, pricing.ActualActual},
		{referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_365F, pricing.Actual365Fixed},
		{referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_360, pricing.Actual360},
		{referencepb.DayCountConvention_DAY_COUNT_CONVENTION_THIRTY_360, pricing.Thirty360},
	}
	for _, c := range cases {
		got, ok := toDayCount(c.wire)
		if !ok {
			t.Errorf("%v did not map", c.wire)
			continue
		}
		if got != c.want {
			t.Errorf("%v mapped to %v, want %v", c.wire, got, c.want)
		}
	}

	// THE NUMBERING GENUINELY DIFFERS, so the test above is not satisfied by a
	// cast that happens to agree. If these ever line up, the explicit map is
	// still correct and this assertion is what says the risk went away.
	if int(referencepb.DayCountConvention_DAY_COUNT_CONVENTION_ACT_ACT) == int(pricing.ActualActual) {
		t.Error("the wire and Go numbering now agree for ACT/ACT — the explicit map is no longer " +
			"guarding against a cast, and this test has stopped proving what it claims")
	}
}

// AN UNSPECIFIED DAY COUNT IS REFUSED, NOT DEFAULTED.
func TestToDayCount_RefusesUnspecified(t *testing.T) {
	if got, ok := toDayCount(referencepb.DayCountConvention_DAY_COUNT_CONVENTION_UNSPECIFIED); ok {
		t.Errorf("UNSPECIFIED mapped to %v — an unset convention became a valid one, and the "+
			"accrual it produces is wrong in a way nothing reports", got)
	}
	b := aBond()
	b.DayCount = referencepb.DayCountConvention_DAY_COUNT_CONVENTION_UNSPECIFIED
	if _, ok := toBondSpec(b); ok {
		t.Error("a bond with no day count converted")
	}
}

// A ZERO-COUPON BOND NEEDS NO FREQUENCY, which BondTerms states outright.
func TestToBondSpec_AZeroCouponBondNeedsNoFrequency(t *testing.T) {
	b := aBond()
	b.CouponRate = bdec(0, 0)
	b.CouponFrequency = referencepb.PaymentFrequency_PAYMENT_FREQUENCY_UNSPECIFIED

	spec, ok := toBondSpec(b)
	if !ok {
		t.Fatal("a zero-coupon bond was refused — its frequency field is documented as ignored")
	}
	if spec.CouponRate != 0 || spec.Frequency != 0 {
		t.Errorf("coupon/frequency = %v/%v, want 0/0", spec.CouponRate, spec.Frequency)
	}
}

// A COUPON BOND WITHOUT A FREQUENCY IS REFUSED, NOT DEFAULTED TO ANNUAL.
//
// Defaulting would halve a semiannual bond's coupon count. The price would land
// close enough to look right and the duration would be materially wrong, which is
// the number the whole FI measure set exists to report.
func TestToFrequency_RefusesAnUnspecifiedFrequencyOnACouponBond(t *testing.T) {
	if _, ok := toFrequency(referencepb.PaymentFrequency_PAYMENT_FREQUENCY_UNSPECIFIED, 0.05); ok {
		t.Error("a 5% coupon with no frequency was accepted")
	}
	for wire, want := range map[referencepb.PaymentFrequency]int{
		referencepb.PaymentFrequency_PAYMENT_FREQUENCY_ANNUAL:     1,
		referencepb.PaymentFrequency_PAYMENT_FREQUENCY_SEMIANNUAL: 2,
		referencepb.PaymentFrequency_PAYMENT_FREQUENCY_QUARTERLY:  4,
		referencepb.PaymentFrequency_PAYMENT_FREQUENCY_MONTHLY:    12,
	} {
		got, ok := toFrequency(wire, 0.05)
		if !ok || got != want {
			t.Errorf("%v -> %v (ok=%v), want %v", wire, got, ok, want)
		}
	}
}

// TERMS THAT CANNOT PRICE ARE REFUSED AT THE BOUNDARY.
//
// Each of these would otherwise reach the bond library, which has no error
// channel: it would return a number. A bond excluded from DV01 silently reduces
// the book's measured rate risk, which is the wrong direction to be quiet in.
func TestToBondSpec_RefusesTermsThatCannotPrice(t *testing.T) {
	cases := map[string]func(*referencepb.BondTerms){
		"no face value":         func(b *referencepb.BondTerms) { b.FaceValue = nil },
		"zero face value":       func(b *referencepb.BondTerms) { b.FaceValue = bdec(0, 0) },
		"negative face value":   func(b *referencepb.BondTerms) { b.FaceValue = bdec(-1000, 0) },
		"negative coupon":       func(b *referencepb.BondTerms) { b.CouponRate = bdec(-5, -2) },
		"no issue date":         func(b *referencepb.BondTerms) { b.IssueDate = nil },
		"no maturity date":      func(b *referencepb.BondTerms) { b.MaturityDate = nil },
		"matures before issue":  func(b *referencepb.BondTerms) { b.MaturityDate = timestamppb.New(bondIssue.AddDate(-1, 0, 0)) },
		"matures at issue":      func(b *referencepb.BondTerms) { b.MaturityDate = timestamppb.New(bondIssue) },
		"no currency":           func(b *referencepb.BondTerms) { b.CurrencyCode = "" },
		"unspecified day count": func(b *referencepb.BondTerms) { b.DayCount = 0 },
		"unspecified frequency": func(b *referencepb.BondTerms) { b.CouponFrequency = 0 },
	}
	for name, mutate := range cases {
		b := aBond()
		mutate(b)
		if _, ok := toBondSpec(b); ok {
			t.Errorf("%s: converted anyway", name)
		}
	}
}

// A FRACTIONAL FACE VALUE SURVIVES THE DECIMAL BOUNDARY, and an issuer id is
// genuinely optional — BondTerms says so, and it is a join key for attribution
// rather than a pricing input.
func TestToBondSpec_FractionalFaceAndOptionalIssuer(t *testing.T) {
	b := aBond()
	b.FaceValue = bdec(100050, -2) // 1000.50
	b.IssuerId = ""

	spec, ok := toBondSpec(b)
	if !ok {
		t.Fatal("a bond with no issuer id was refused — the field is optional")
	}
	if spec.Face != 1000.50 {
		t.Errorf("face = %v, want 1000.50", spec.Face)
	}
}
