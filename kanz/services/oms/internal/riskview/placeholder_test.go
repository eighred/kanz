package riskview

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
)

// A PLACEHOLDER MODEL DOES NOT GATE ORDER ADMISSION (#1037).
//
// compute.VaR99 is 1%×gross — an illustrative constant, and the answer every
// deployment with no RISK_ENGINE_MARKETDATA_DATABASE_URL serves, which today is
// every deployment in infra/. It carries no coverage (there is no provider to
// decline), so the ExcludedCount arm below cannot see it, and it announces a
// number on the same subject as a calibrated one. For a leveraged or volatile
// book 1% of gross sits BELOW a real one-day 99% VaR, so a mandate limit checked
// against it passes orders that the real number would refuse.
//
// The answer is the one riskview already gives for coverage: leave the measure
// UNKNOWN, which the mandate rules fail closed on. NOT annotate-and-let-the-rule-
// decide — a rule comparing an illustrative constant against a limit is not
// conservative, it is arbitrary.

// measuresWithProvenance builds an announcement whose measures declare a method.
func measuresWithProvenance(t *testing.T, pf string, asOf time.Time, name string, v int64, method string) []byte {
	t.Helper()
	m := &domainpb.RiskMeasure{Name: name, Value: &commonpb.Decimal{Coefficient: v}}
	if method != "" {
		m.Provenance = &domainpb.MeasureProvenance{Method: method}
	}
	set := &domainpb.RiskMeasureSet{
		PortfolioId: pf,
		AsOf:        timestamppb.New(asOf),
		Measures:    []*domainpb.RiskMeasure{m},
	}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHandle_PlaceholderMeasureIsLeftUnknown(t *testing.T) {
	var gotPF, gotMeasure, gotMethod string
	v := New(
		WithClock(func() time.Time { return t0 }),
		WithOnPlaceholder(func(portfolio, measure, method string) {
			gotPF, gotMeasure, gotMethod = portfolio, measure, method
		}),
	)

	if err := v.Handle(context.Background(), nil,
		measuresWithProvenance(t, "PF1", t0, "VaR99", 250, "placeholder_1pct_gross")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, ok := v.Measure("PF1", "VaR99"); ok {
		t.Fatal("the 1%×gross placeholder was folded and will gate a mandate's VaR limit — " +
			"an illustrative constant admitting orders a real VaR would refuse")
	}
	if gotPF != "PF1" || gotMeasure != "VaR99" || gotMethod != "placeholder_1pct_gross" {
		t.Errorf("onPlaceholder observed (%q,%q,%q), want (PF1,VaR99,placeholder_1pct_gross) — "+
			"the refusal is indistinguishable from a quiet engine unless it is reported",
			gotPF, gotMeasure, gotMethod)
	}
}

// THE CALIBRATED MODEL MUST STILL GATE. A refusal that also swallowed historical
// simulation would turn the fix into an outage: every risk-limit mandate would
// refuse forever on a correctly configured engine.
func TestHandle_PlaceholderRefusalDoesNotTouchACalibratedModel(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil,
		measuresWithProvenance(t, "PF1", t0, "VaR99", 250, "historical_simulation")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok := v.Measure("PF1", "VaR99")
	if !ok {
		t.Fatal("historical-simulation VaR99 was refused — the gate now fails closed on the " +
			"one configuration that computes a real number")
	}
	if got.RatString() != "250" {
		t.Errorf("VaR99 = %s, want 250", got.RatString())
	}
}

// ABSENT PROVENANCE IS NOT A PLACEHOLDER, for the same reason absent coverage is
// not an exclusion: a measure that does not report its model has said nothing,
// and every measure published before #1037 says nothing. Refusing on absence
// would fail every gate closed on the first deploy of this change.
func TestHandle_PlaceholderAbsentProvenanceStillFolds(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil,
		measuresWithProvenance(t, "PF1", t0, "VaR99", 250, "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := v.Measure("PF1", "VaR99"); !ok {
		t.Fatal("a measure carrying no provenance was refused — every measure published " +
			"before this change carries none")
	}
}

// THE NET-EXPOSURE DELTA IS THE SAME DEFECT UNDER ANOTHER NAME, and it is served
// on EVERY deployment, not just the ones missing a DSN.
func TestHandle_PlaceholderDeltaIsLeftUnknown(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil,
		measuresWithProvenance(t, "PF1", t0, "Delta", 900, "net_exposure_placeholder")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := v.Measure("PF1", "Delta"); ok {
		t.Fatal("the net-exposure Delta placeholder was folded and can gate a mandate")
	}
}
