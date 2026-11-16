package termsource

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/terms"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/scenario"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func invalidOptionSemantics() []struct {
	name string
	mut  func(*referencepb.OptionTerms)
} {
	return []struct {
		name string
		mut  func(*referencepb.OptionTerms)
	}{
		{"unspecified exercise", func(o *referencepb.OptionTerms) {
			o.ExerciseStyle = referencepb.ExerciseStyle_EXERCISE_STYLE_UNSPECIFIED
		}},
		{"unknown exercise", func(o *referencepb.OptionTerms) { o.ExerciseStyle = referencepb.ExerciseStyle(99) }},
		{"negative exercise", func(o *referencepb.OptionTerms) { o.ExerciseStyle = referencepb.ExerciseStyle(-1) }},
		{"missing expiry", func(o *referencepb.OptionTerms) { o.Expiry = nil }},
		{"negative nanos", func(o *referencepb.OptionTerms) { o.Expiry.Nanos = -1 }},
		{"overflow nanos", func(o *referencepb.OptionTerms) { o.Expiry.Nanos = 1_000_000_000 }},
		{"below timestamp range", func(o *referencepb.OptionTerms) { o.Expiry.Seconds = -62_135_596_801 }},
		{"above timestamp range", func(o *referencepb.OptionTerms) { o.Expiry.Seconds = 253_402_300_800 }},
		{"empty underlying", func(o *referencepb.OptionTerms) { o.UnderlyingId = "" }},
		{"blank underlying", func(o *referencepb.OptionTerms) { o.UnderlyingId = " \t\n" }},
		{"unspecified option type", func(o *referencepb.OptionTerms) { o.OptionType = referencepb.OptionType_OPTION_TYPE_UNSPECIFIED }},
		{"unknown option type", func(o *referencepb.OptionTerms) { o.OptionType = referencepb.OptionType(99) }},
	}
}

func TestOptionRequiredSemanticsResolveUnusable(t *testing.T) {
	for _, tc := range invalidOptionSemantics() {
		t.Run(tc.name, func(t *testing.T) {
			rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
			tc.mut(rec.Terms.GetOption())
			before := proto.Clone(rec.Terms)
			spec, resolution, err := resolveOptionRecord(rec)
			if err != nil || resolution != compute.TermsUnusable || spec != (compute.OptionSpec{}) {
				t.Fatalf("invalid semantics became usable/linear: resolution=%v spec=%+v error=%v", resolution, spec, err)
			}
			if !proto.Equal(before, rec.Terms) {
				t.Fatal("refusal rewrote canonical terms")
			}
		})
	}
}

type semanticRecordProvider struct{ rec terms.Record }

func (p semanticRecordProvider) OptionTerms(context.Context, string, time.Time) (compute.OptionSpec, compute.TermsResolution, error) {
	return resolveOptionRecord(p.rec)
}

type semanticMarket struct{ calls int }

func (m *semanticMarket) Spot(context.Context, string, time.Time) (float64, bool) {
	m.calls++
	return 100, true
}

func (m *semanticMarket) Vol(context.Context, string, float64, float64, time.Time) (float64, bool) {
	m.calls++
	return 0.2, true
}

func semanticPortfolio(id string, at time.Time) *domain.Portfolio {
	p := domain.NewPortfolio("option-semantics", "USD")
	p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), AsOf: at, Quantity: dec(1, 0),
		MarketValue: &commonpb.Money{Amount: dec(100, 0), CurrencyCode: "USD"}})
	return p
}

func assertSemanticRefusalCoverage(t *testing.T, provider compute.TermsProvider, id string, at time.Time) {
	t.Helper()
	p := semanticPortfolio(id, at)
	market := &semanticMarket{}
	providers := compute.GreeksProviders{Terms: provider, Spot: market, Vol: market, Curve: pricing.FlatCurve(0.03)}
	registry := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), registry, providers)
	measures := compute.ComputeMeasures(p, registry, nil)
	for _, name := range []v1.MeasureName{compute.MeasureDelta, compute.MeasureGamma, compute.MeasureVega, compute.MeasureTheta, compute.MeasureRho} {
		m, ok := measures.Lookup(name)
		if !ok || m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != compute.SkipInvalidOptionTerms {
			t.Fatalf("invalid semantics produced clean %s: %+v", name, m)
		}
	}
	rv := compute.NewRevaluer(providers)
	pos, _ := p.Position(domain.InstrumentID(id))
	result := rv.RevalueOption(context.Background(), id, at, pos.MarketValue, compute.RevalShocks{PriceFrac: -.2})
	if result.Status != compute.RevalUnassessed || result.Value != nil || result.Reason != compute.SkipInvalidOptionTerms {
		t.Fatalf("invalid semantics produced priced/linear disposition: %+v", result)
	}
	projected, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.ParallelShift{Pct: dec(-2, -1)}}, nil, rv)
	if cov.ExcludedCount != 1 || cov.Contributed != 0 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != compute.SkipInvalidOptionTerms {
		t.Fatalf("invalid semantics produced clean scenario: %+v", cov)
	}
	// Retain the existing explicitly degraded linear approximation: $100 less
	// one fifth is $80, while the source book remains $100.
	m, _ := projected.Lookup(compute.MeasureGrossExposure)
	if m.Value.GetCoefficient() != 80_000_000 || m.Value.GetExponent() != -6 || pos.MarketValue.Amount.Coefficient != 100 {
		t.Fatal("degraded scenario changed its arithmetic or mutated the source book")
	}
	if market.calls != 0 {
		t.Fatal("unusable option semantics reached spot/vol pricing")
	}
}

func TestOptionRequiredSemanticsReachGreekAndScenarioCoverage(t *testing.T) {
	for _, tc := range invalidOptionSemantics() {
		t.Run(tc.name, func(t *testing.T) {
			rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
			tc.mut(rec.Terms.GetOption())
			assertSemanticRefusalCoverage(t, semanticRecordProvider{rec}, "OPTION", t0)
		})
	}
}

func TestOptionRequiredSemanticsPreserveCanonicalMappings(t *testing.T) {
	for _, style := range []referencepb.ExerciseStyle{referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN, referencepb.ExerciseStyle_EXERCISE_STYLE_AMERICAN} {
		for _, typ := range []referencepb.OptionType{referencepb.OptionType_OPTION_TYPE_CALL, referencepb.OptionType_OPTION_TYPE_PUT} {
			rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
			o := rec.Terms.GetOption()
			o.ExerciseStyle, o.OptionType = style, typ
			o.Strike, o.ContractMultiplier = dec(1005, -1), dec(25, -1)
			spec, resolution, err := resolveOptionRecord(rec)
			wantExercise := pricing.European
			if style == referencepb.ExerciseStyle_EXERCISE_STYLE_AMERICAN {
				wantExercise = pricing.American
			}
			wantType := pricing.Call
			if typ == referencepb.OptionType_OPTION_TYPE_PUT {
				wantType = pricing.Put
			}
			if err != nil || resolution != compute.TermsResolved || spec.Exercise != wantExercise || spec.Type != wantType || spec.UnderlyingID != "UNDERLYING" || spec.Strike != 100.5 || spec.Multiplier != 2.5 || !spec.Expiry.Equal(o.Expiry.AsTime()) {
				t.Fatalf("valid canonical mapping changed: %+v resolution=%v error=%v", spec, resolution, err)
			}
		}
	}
	for _, stamp := range []*timestamppb.Timestamp{{Seconds: -62_135_596_800}, {}, {Seconds: 253_402_300_799, Nanos: 999_999_999}} {
		rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
		rec.Terms.GetOption().Expiry = stamp
		if spec, resolution, err := resolveOptionRecord(rec); err != nil || resolution != compute.TermsResolved || !spec.Expiry.Equal(stamp.AsTime()) {
			t.Fatalf("valid expiry instant refused: %v resolution=%v error=%v", stamp, resolution, err)
		}
	}
	// A valid expired instant remains resolved; consumers report expiration.
	rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
	rec.Terms.GetOption().Expiry = timestamppb.New(t0)
	market := &semanticMarket{}
	rv := compute.NewRevaluer(compute.GreeksProviders{Terms: semanticRecordProvider{rec}, Spot: market, Vol: market})
	if got := rv.RevalueOption(context.Background(), "OPTION", t0, &commonpb.Money{Amount: dec(100, 0), CurrencyCode: "USD"}, compute.RevalShocks{}); got.Status != compute.RevalUnassessed || got.Reason != compute.SkipExpired {
		t.Fatalf("valid expiration was conflated with malformed terms: %+v", got)
	}
}
