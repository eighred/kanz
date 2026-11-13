package termsource

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

type bondTimeCase struct {
	name string
	bond *referencepb.BondTerms
}

func malformedBondTimes() []bondTimeCase {
	var cases []bondTimeCase
	for _, field := range []string{"issue", "maturity"} {
		for name, stamp := range map[string]*timestamppb.Timestamp{
			"negative one nanos": {Seconds: bondIssue.Unix(), Nanos: -1},
			"negative nanos":     {Seconds: bondIssue.Unix() + 1, Nanos: -1_000_000_000},
			"overflow nanos":     {Seconds: bondIssue.Unix(), Nanos: 1_000_000_000},
			"before year one":    {Seconds: -62135596801},
			"after year 9999":    {Seconds: 253402300800},
		} {
			b := aBond()
			// Keep the normal-year maturity variants after issue, so ordering
			// alone cannot accidentally establish timestamp validity.
			if field == "maturity" && name != "before year one" && name != "after year 9999" {
				stamp.Seconds += b.MaturityDate.Seconds - b.IssueDate.Seconds
			}
			if field == "issue" {
				b.IssueDate = stamp
			} else {
				b.MaturityDate = stamp
			}
			cases = append(cases, bondTimeCase{field + "/" + name, b})
		}
	}
	return cases
}

func TestBondTimestampDirectConversionRefusesMalformed(t *testing.T) {
	for _, tc := range malformedBondTimes() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.bond.IssueDate.CheckValid() == nil && tc.bond.MaturityDate.CheckValid() == nil {
				t.Fatal("fixture is not a malformed protobuf timestamp")
			}
			if _, ok := toBondSpec(tc.bond); ok {
				t.Fatal("malformed bond timestamp was normalized into usable terms")
			}
			if _, ok := BondSpecFromTerms(tc.bond); ok {
				t.Fatal("direct conformance/capture conversion accepted malformed terms")
			}
		})
	}
}

func TestBondTimestampValidCounterpartsAndBoundaries(t *testing.T) {
	// Independent, explicitly normalized counterparts of -1 and 1e9 nanos.
	for _, field := range []string{"issue", "maturity"} {
		for _, delta := range []time.Duration{-time.Nanosecond, time.Second} {
			t.Run(field+"/"+delta.String(), func(t *testing.T) {
				b := aBond()
				want := bondIssue.Add(delta)
				if field == "maturity" {
					want = bondIssue.AddDate(5, 0, 0).Add(delta)
					b.MaturityDate = timestamppb.New(want)
				} else {
					b.IssueDate = timestamppb.New(want)
				}
				spec, ok := BondSpecFromTerms(b)
				if !ok || !spec.Issue.Equal(b.IssueDate.AsTime()) || !spec.Maturity.Equal(b.MaturityDate.AsTime()) || spec.CouponRate != .05 || spec.Frequency != 2 {
					t.Fatalf("valid counterpart changed: %+v ok=%v", spec, ok)
				}
			})
		}
	}
	// Conversion only: one-second bonds near each canonical Timestamp limit,
	// never an enormous cashflow schedule or a new maturity policy.
	for name, dates := range map[string][2]*timestamppb.Timestamp{
		"minimum": {{Seconds: -62135596800}, {Seconds: -62135596799}},
		"maximum": {{Seconds: 253402300798, Nanos: 999999999}, {Seconds: 253402300799, Nanos: 999999999}},
	} {
		t.Run(name, func(t *testing.T) {
			b := aBond()
			b.IssueDate, b.MaturityDate = dates[0], dates[1]
			if b.IssueDate.CheckValid() != nil || b.MaturityDate.CheckValid() != nil {
				t.Fatal("invalid boundary control")
			}
			if spec, ok := BondSpecFromTerms(b); !ok || !spec.Issue.Equal(dates[0].AsTime()) || !spec.Maturity.Equal(dates[1].AsTime()) {
				t.Fatalf("valid boundary precision changed: %+v ok=%v", spec, ok)
			}
		})
	}
}

// A conversion-only adapter for ungated FI tests. Actual Provider/store and
// observer behavior are exercised separately by the PostgreSQL parents.
type bondTimeConversion struct{ bond *referencepb.BondTerms }

func (p bondTimeConversion) BondTerms(context.Context, string, time.Time) (compute.BondSpec, compute.TermsResolution) {
	spec, ok := BondSpecFromTerms(p.bond)
	if !ok {
		return compute.BondSpec{}, compute.TermsUnusable
	}
	return spec, compute.TermsResolved
}

type bondTimeCurve struct {
	curve *curve.Curve
	calls int
}

func (c *bondTimeCurve) Curve(context.Context, string, time.Time) (*curve.Curve, bool) {
	c.calls++
	return c.curve, c.curve != nil
}

func bondTimePortfolio(id string, at time.Time) *domain.Portfolio {
	p := domain.NewPortfolio("bond-time", "USD")
	p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), Quantity: bdec(2, 0), MarketValue: &commonpb.Money{Amount: bdec(2000, 0), CurrencyCode: "USD"}, AsOf: at})
	return p
}

var bondTimeMeasures = []v1.MeasureName{compute.MeasureDV01, compute.MeasureDuration, compute.MeasureConvexity, compute.MeasureSpreadDuration}

func assertBondTimestampFIRefusal(t *testing.T, terms compute.BondTermsProvider, id string, at time.Time) {
	t.Helper()
	// An absent curve makes a missing timestamp check fail without ever pricing
	// an out-of-range-date schedule. The exclusion must precede any curve lookup.
	c := &bondTimeCurve{}
	captures := 0
	providers := compute.FIProviders{Terms: terms, Curve: c, OnBondRisk: func(compute.BondRiskCapture) { captures++ }}
	r := compute.DefaultRegistry()
	compute.RegisterFIRisk(context.Background(), r, providers)
	ms := compute.ComputeMeasures(bondTimePortfolio(id, at), r, nil)
	for _, name := range bondTimeMeasures {
		m, ok := ms.Lookup(name)
		if !ok || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].InstrumentID != v1.InstrumentID(id) || m.Coverage.Exclusions[0].Reason != compute.SkipNoTerms {
			t.Fatalf("malformed dates produced a clean/wrong %s disposition: %+v", name, m)
		}
	}
	if got, ok := compute.NewBondRevaluer(providers).RevalueBond(context.Background(), id, at, &commonpb.Money{Amount: bdec(2000, 0), CurrencyCode: "USD"}, curve.Parallel{Bp: 100}); ok || got != nil {
		t.Fatalf("malformed dates reached bond repricing: %+v ok=%v", got, ok)
	}
	if c.calls != 0 || captures != 0 {
		t.Fatal("unusable terms reached curve lookup or numerical capture")
	}
}

func TestBondTimestampRefusalReachesFICoverage(t *testing.T) {
	for _, tc := range malformedBondTimes() {
		t.Run(tc.name, func(t *testing.T) {
			assertBondTimestampFIRefusal(t, bondTimeConversion{tc.bond}, "BOND", bondIssue)
		})
	}
}

func bondTimeZero(at time.Time) *referencepb.BondTerms {
	b := aBond()
	b.IssueDate, b.MaturityDate = timestamppb.New(at), timestamppb.New(at.AddDate(1, 0, 0))
	b.CouponRate, b.CouponFrequency = bdec(0, 0), referencepb.PaymentFrequency_PAYMENT_FREQUENCY_UNSPECIFIED
	return b
}

func assertBondTimestampZeroOracle(t *testing.T, terms compute.BondTermsProvider, id string, at time.Time) {
	t.Helper()
	c, err := curve.NewZeroCurve([]float64{1}, []float64{.03}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	var captures []compute.BondRiskCapture
	r := compute.DefaultRegistry()
	compute.RegisterFIRisk(context.Background(), r, compute.FIProviders{Terms: terms, Curve: &bondTimeCurve{curve: c}, OnBondRisk: func(c compute.BondRiskCapture) { captures = append(captures, c) }})
	ms := compute.ComputeMeasures(bondTimePortfolio(id, at), r, nil)
	// One 1000 redemption exactly 365 days away, continuous 3%, symmetric 1bp
	// repricing: P=1000 exp(-.03), DV01=P sinh(1e-4), without the Go scheduler.
	price := 1000 * math.Exp(-.03)
	dv := price * math.Sinh(1e-4)
	if len(captures) != 1 || math.Abs(captures[0].Result.DirtyPrice-price) > 1e-9 || math.Abs(captures[0].Result.DV01-dv) > 1e-10 {
		t.Fatalf("zero-coupon price/DV01 differs from analytic redemption: %+v", captures)
	}
	for _, name := range bondTimeMeasures {
		m, ok := ms.Lookup(name)
		if !ok || m.Coverage.Contributed != 1 || m.Coverage.ExcludedCount != 0 {
			t.Fatalf("valid zero coupon was excluded: %s %+v", name, m)
		}
		// The declared wire contract is cents for dollar DV01 and 1e-4 for
		// duration/convexity. These analytic values round to exactly 19 cents
		// and 1.0000 respectively; raw price/DV01 are checked above separately.
		wantCoefficient, wantExponent := int64(10000), int32(-4)
		if name == compute.MeasureDV01 {
			wantCoefficient, wantExponent = 19, -2 // two bonds
		}
		if m.Value.GetCoefficient() != wantCoefficient || m.Value.GetExponent() != wantExponent {
			t.Fatalf("%s wire value=%v, want coefficient=%d exponent=%d", name, m.Value, wantCoefficient, wantExponent)
		}
	}
}

func TestBondTimestampZeroCouponNumericalOracle(t *testing.T) {
	assertBondTimestampZeroOracle(t, bondTimeConversion{bondTimeZero(bondIssue)}, "ZERO", bondIssue)
}
