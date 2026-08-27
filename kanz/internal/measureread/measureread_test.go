package measureread

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// unconvertible is a Decimal outside dec.InDomain — the exponent is far outside
// the representable range, so dec.Float64 reports ok=false. It stands in for
// every way a value can fail to convert; the projection must not turn any of
// them into a zero.
func unconvertible() *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000}
}

func measure(name string, value *commonpb.Decimal, cov *domainpb.InputCoverage) *domainpb.RiskMeasure {
	return &domainpb.RiskMeasure{Name: name, Value: value, Coverage: cov}
}

func measuresResponse(ms []*domainpb.RiskMeasure, flags []querypb.QualityFlag, asOf *timestamppb.Timestamp) *querypb.MeasuresResponse {
	return &querypb.MeasuresResponse{
		PortfolioId:  "pf-1",
		AsOf:         asOf,
		Set:          &domainpb.RiskMeasureSet{PortfolioId: "pf-1", Measures: ms},
		QualityFlags: flags,
	}
}

func find(t *testing.T, s Set, name string) Measure {
	t.Helper()
	for _, m := range s.Measures {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("measure %q is absent from the projection (%d measures) — a measure that cannot be "+
		"stated must still be REPORTED as unavailable, never dropped", name, len(s.Measures))
	return Measure{}
}

// THE DEFECT THIS PACKAGE EXISTS FOR (#757).
//
// The FI family is registered in production over a contract-terms store with no
// production writer, so DV01 arrives as a zero computed over zero bonds. Before
// this projection both agent planes rendered it as `DV01 = 0`, which an agent
// states as "your book carries no interest-rate risk".
func TestAMeasureComputedOverNothingIsNotStatedAsZero(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("DV01", d(0, 0), &domainpb.InputCoverage{
			Contributed:   0,
			ExcludedCount: 12,
			Exclusions: []*domainpb.InputExclusion{
				{InstrumentId: "BOND-1", Reason: "no_terms"},
			},
		}),
	}, nil, nil)

	got := find(t, ProjectMeasures(resp), "DV01")

	if got.Status != StatusUnavailable {
		t.Fatalf("DV01 status = %q, want %q — it was computed over 0 of 12 positions, so the zero "+
			"is an active claim about a book nothing was read from", got.Status, StatusUnavailable)
	}
	if got.Value != nil {
		t.Errorf("DV01 carries a value (%v) despite being unavailable — the whole defect is that a "+
			"withheld measure must carry NO number for an agent to restate", *got.Value)
	}
	if got.Reason == "" {
		t.Error("DV01 is unavailable with no reason — a refusal an operator cannot act on is a silent drop")
	}
	if !got.Coverage.Reported || got.Coverage.Excluded != 12 || got.Coverage.Contributed != 0 {
		t.Errorf("DV01 coverage = %+v, want reported with contributed=0 excluded=12 — the evidence "+
			"#527 put on the wire must reach the caller", got.Coverage)
	}
	if len(got.Coverage.ExclusionReasons) == 0 {
		t.Error("DV01 carries no exclusion reason — 'no_terms' is what tells an operator which store is empty")
	}
}

// A PARTIAL BOOK IS STILL A REFUSAL. domain.v1.InputCoverage — "a caller gating on a
// money measure must treat a non-zero excluded_count as a REFUSAL TO ANSWER, not
// as an annotation on a good number", because dropping positions moves a ratio
// or a quantile in an unknown direction.
func TestAPartiallyCoveredMeasureIsWithheldRatherThanAnnotated(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("VaR99", d(1234, -2), &domainpb.InputCoverage{Contributed: 499, ExcludedCount: 1}),
	}, nil, nil)

	got := find(t, ProjectMeasures(resp), "VaR99")

	if got.Status != StatusUnavailable || got.Value != nil {
		t.Fatalf("VaR99 = %+v, want withheld — one excluded leg of a hedged pair RAISES a quantile, "+
			"so a 499-of-500 number is not a smaller number, it is an unknown one", got)
	}
	if got.Coverage.Contributed != 499 || got.Coverage.Excluded != 1 {
		t.Errorf("VaR99 coverage = %+v, want contributed=499 excluded=1 — withholding the value must "+
			"not also withhold the shape of the gap", got.Coverage)
	}
}

// COVERAGE ABSENT IS NOT COVERAGE CLEAN. domain.v1.RiskMeasure.coverage — "ABSENT means this
// measure does not report input coverage — it does NOT mean everything
// resolved."
func TestAbsentCoverageIsReportedAsNotReportedNotAsClean(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("GrossExposure", d(500, 0), nil),
	}, nil, nil)

	got := find(t, ProjectMeasures(resp), "GrossExposure")

	if got.Status != StatusMeasured {
		t.Fatalf("GrossExposure status = %q, want %q — a measure that does not report coverage is "+
			"still the engine's answer, and withholding every one of them would empty the plane",
			got.Status, StatusMeasured)
	}
	if got.Coverage.Reported {
		t.Error("GrossExposure reports coverage it never carried — presence is the signal, and " +
			"synthesising a clean record is exactly the conflation InputCoverage is a message to break")
	}
	if got.Value == nil || *got.Value != 500 {
		t.Errorf("GrossExposure value = %v, want 500", got.Value)
	}
}

// AN UNREADABLE VALUE IS NOT A ZERO. dec.Float64's bool is not decoration; the
// planes used to discard it with Float64Or(v, 0).
func TestAnUnconvertibleValueIsWithheldRatherThanZeroed(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("Delta", unconvertible(), nil),
	}, nil, nil)

	got := find(t, ProjectMeasures(resp), "Delta")

	if got.Status != StatusUnavailable || got.Value != nil {
		t.Fatalf("Delta = %+v, want withheld — Float64Or(v, 0) is this repository's own named marker "+
			"for a confident zero (internal/dec/float.go)", got)
	}
}

// CURRENCY_EXCLUDED IS A RESPONSE-WIDE REFUSAL. query.v1 QUALITY_FLAG_CURRENCY_EXCLUDED — "a gate
// that must not under-report MUST refuse to act on a response carrying this flag
// rather than treat it as a number", and no direction claim may be made.
func TestCurrencyExcludedWithholdsEveryValueOnTheResponse(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("GrossExposure", d(500, 0), nil),
		measure("VaR99", d(12, 0), &domainpb.InputCoverage{Contributed: 10}),
	}, []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_CURRENCY_EXCLUDED}, nil)

	set := ProjectMeasures(resp)

	for _, name := range []string{"GrossExposure", "VaR99"} {
		got := find(t, set, name)
		if got.Status != StatusUnavailable || got.Value != nil {
			t.Errorf("%s = %+v, want withheld — the response was computed over a currency SUBSET of "+
				"the book and a base-currency measure moves either way when a leg is dropped", name, got)
		}
	}
	if len(set.QualityFlags) != 1 || set.QualityFlags[0] != "CURRENCY_EXCLUDED" {
		t.Errorf("quality flags = %v, want [CURRENCY_EXCLUDED] named on the response", set.QualityFlags)
	}
}

// AN UNNAMED FLAG IS AN UNKNOWN, AND A CRITICAL UNKNOWN FAILS CLOSED. grpcsrv
// emits QUALITY_FLAG_UNSPECIFIED for a flag it could not map, and says in as many
// words that a caller must not read it as "fine".
func TestAnUnspecifiedQualityFlagFailsClosed(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("GrossExposure", d(500, 0), nil),
	}, []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_UNSPECIFIED}, nil)

	set := ProjectMeasures(resp)

	if got := find(t, set, "GrossExposure"); got.Status != StatusUnavailable {
		t.Fatalf("GrossExposure status = %q under an UNSPECIFIED flag, want %q — the engine marked "+
			"something it could not name, which is an unknown, not an all-clear", got.Status, StatusUnavailable)
	}
}

// DEGRADED AND STALE DO NOT WITHHOLD. The values are real, just cached or old;
// as_of and the named flag say precisely that, and emptying the plane during a
// degradation is emptying it exactly when somebody is investigating one.
func TestDegradedAndStaleAreNamedButDoNotWithhold(t *testing.T) {
	asOf := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("GrossExposure", d(500, 0), nil),
	}, []querypb.QualityFlag{
		querypb.QualityFlag_QUALITY_FLAG_DEGRADED,
		querypb.QualityFlag_QUALITY_FLAG_STALE,
	}, timestamppb.New(asOf))

	set := ProjectMeasures(resp)

	if got := find(t, set, "GrossExposure"); got.Status != StatusMeasured || got.Value == nil {
		t.Fatalf("GrossExposure = %+v under DEGRADED/STALE, want measured — a cached number is a "+
			"number, and the flag plus as_of is what makes it honest", got)
	}
	if len(set.QualityFlags) != 2 {
		t.Fatalf("quality flags = %v, want both named", set.QualityFlags)
	}
	if set.AsOf == nil || !set.AsOf.Equal(asOf) {
		t.Errorf("as_of = %v, want %v — without it an agent cannot tell current state from state "+
			"folded hours ago, which is the whole content of STALE", set.AsOf, asOf)
	}
}

// INPUTS_UNRESOLVED IS THE SET-LEVEL ECHO OF PER-MEASURE COVERAGE. #509 was filed
// because the set-level flag alone forced a caller to refuse everything or
// nothing; re-withholding the whole response on it would spend exactly what that
// issue bought.
func TestInputsUnresolvedWithholdsOnlyTheMeasuresItsCoverageNames(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("GrossExposure", d(500, 0), &domainpb.InputCoverage{Contributed: 500}),
		measure("DV01", d(0, 0), &domainpb.InputCoverage{Contributed: 0, ExcludedCount: 12}),
	}, []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED}, nil)

	set := ProjectMeasures(resp)

	if got := find(t, set, "GrossExposure"); got.Status != StatusMeasured {
		t.Errorf("GrossExposure status = %q, want %q — refusing the sound measure beside the "+
			"degraded one is the all-or-nothing #509 removed", got.Status, StatusMeasured)
	}
	if got := find(t, set, "DV01"); got.Status != StatusUnavailable {
		t.Errorf("DV01 status = %q, want %q", got.Status, StatusUnavailable)
	}
}

// AN ABSENT as_of IS SAID, NOT INVENTED. A zero timestamp rendered as a real
// instant is a lie about how current the state is.
func TestAnAbsentAsOfIsNilRatherThanTheZeroInstant(t *testing.T) {
	set := ProjectMeasures(measuresResponse(nil, nil, nil))
	if set.AsOf != nil {
		t.Errorf("as_of = %v for a response that carried none, want nil", set.AsOf)
	}
	if set.QualityFlags == nil {
		t.Error("quality flags is nil rather than empty — the projection must always carry the " +
			"field so a reader is never left inferring trust from a missing key (#675)")
	}
}

// A NIL RESPONSE IS NOT AN EMPTY BOOK.
func TestANilResponseProjectsToNothingRatherThanPanicking(t *testing.T) {
	set := ProjectMeasures(nil)
	if len(set.Measures) != 0 {
		t.Errorf("measures = %v for a nil response", set.Measures)
	}
	if set.QualityFlags == nil {
		t.Error("quality flags is nil for a nil response, want empty")
	}
}

// DETERMINISTIC ORDER. Both planes render this list; a map iteration would make
// two identical reads produce two different agent contexts.
func TestMeasuresAreOrderedByName(t *testing.T) {
	resp := measuresResponse([]*domainpb.RiskMeasure{
		measure("VaR99", d(1, 0), nil),
		measure("Delta", d(2, 0), nil),
		measure("GrossExposure", d(3, 0), nil),
	}, nil, nil)

	set := ProjectMeasures(resp)
	want := []string{"Delta", "GrossExposure", "VaR99"}
	for i, w := range want {
		if set.Measures[i].Name != w {
			t.Fatalf("measure %d = %q, want %q (order: %v)", i, set.Measures[i].Name, w, want)
		}
	}
}

// THE UNCERTAINTY BAND TRAVELS WHEN IT EXISTS, AND IS ABSENT WHEN IT DOES NOT.
// domain.v1.RiskMeasure.uncertainty_abs — "Absent (no value set) means no uncertainty was propagated —
// distinct from 'zero uncertainty', which would be a calibrated point estimate."
func TestUncertaintyIsCarriedWhenPresentAndOmittedWhenAbsent(t *testing.T) {
	withBand := &domainpb.RiskMeasure{Name: "VaR99", Value: d(100, 0), UncertaintyAbs: d(5, 0)}
	without := &domainpb.RiskMeasure{Name: "Delta", Value: d(100, 0)}
	set := ProjectMeasures(measuresResponse([]*domainpb.RiskMeasure{withBand, without}, nil, nil))

	if got := find(t, set, "VaR99"); got.UncertaintyAbs == nil || *got.UncertaintyAbs != 5 {
		t.Errorf("VaR99 uncertainty = %v, want 5", got.UncertaintyAbs)
	}
	if got := find(t, set, "Delta"); got.UncertaintyAbs != nil {
		t.Errorf("Delta uncertainty = %v, want absent — a zero band claims a calibrated point "+
			"estimate the engine never made", *got.UncertaintyAbs)
	}
}

// ---- exposure ----

func exposureResponse(es []*domainpb.ExposureState, flags []querypb.QualityFlag) *querypb.ExposureResponse {
	return &querypb.ExposureResponse{
		PortfolioId:  "pf-1",
		Set:          &domainpb.ExposureSet{PortfolioId: "pf-1", Exposures: es},
		QualityFlags: flags,
	}
}

func exposure(dim domainpb.ExposureDimension, bucket string, net *commonpb.Decimal) *domainpb.ExposureState {
	return &domainpb.ExposureState{
		PortfolioId: "pf-1",
		Dimension:   dim,
		Bucket:      bucket,
		Net:         &commonpb.Money{Amount: net, CurrencyCode: "USD"},
	}
}

func TestAnUnconvertibleExposureIsWithheldRatherThanZeroed(t *testing.T) {
	set := ProjectExposure(exposureResponse([]*domainpb.ExposureState{
		exposure(domainpb.ExposureDimension_EXPOSURE_DIMENSION_ASSET_CLASS, "EQUITY", unconvertible()),
		exposure(domainpb.ExposureDimension_EXPOSURE_DIMENSION_ASSET_CLASS, "BOND", d(250, 0)),
	}, nil))

	bad := find(t, set, "EXPOSURE_DIMENSION_ASSET_CLASS/EQUITY")
	if bad.Status != StatusUnavailable || bad.Value != nil {
		t.Errorf("EQUITY exposure = %+v, want withheld — a net exposure that could not be read is "+
			"not a flat book", bad)
	}
	good := find(t, set, "EXPOSURE_DIMENSION_ASSET_CLASS/BOND")
	if good.Status != StatusMeasured || good.Value == nil || *good.Value != 250 {
		t.Errorf("BOND exposure = %+v, want 250 measured", good)
	}
}

func TestCurrencyExcludedWithholdsExposureToo(t *testing.T) {
	set := ProjectExposure(exposureResponse([]*domainpb.ExposureState{
		exposure(domainpb.ExposureDimension_EXPOSURE_DIMENSION_ASSET_CLASS, "EQUITY", d(500, 0)),
	}, []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_CURRENCY_EXCLUDED}))

	got := find(t, set, "EXPOSURE_DIMENSION_ASSET_CLASS/EQUITY")
	if got.Status != StatusUnavailable {
		t.Errorf("EQUITY exposure status = %q under CURRENCY_EXCLUDED, want %q — the currency "+
			"dimension is precisely the one a dropped currency distorts", got.Status, StatusUnavailable)
	}
}

// A SCENARIO INHERITS THE COVERAGE OF WHAT IT WAS SHOCKED FROM. Moving no
// capital is not the same as stating nothing: a what-if over a book whose terms
// never loaded is exactly as empty as the measure it was projected from.
func TestAScenarioProjectionWithholdsOnTheSameEvidence(t *testing.T) {
	resp := &querypb.EvaluateScenarioResponse{
		PortfolioId: "pf-1",
		Projected: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{
			measure("DV01", d(0, 0), &domainpb.InputCoverage{Contributed: 0, ExcludedCount: 7}),
			measure("GrossExposure", d(500, 0), nil),
		}},
	}

	set := ProjectScenario(resp)

	if got := find(t, set, "DV01"); got.Status != StatusUnavailable || got.Value != nil {
		t.Errorf("projected DV01 = %+v, want withheld — the shock was applied to nothing", got)
	}
	if got := find(t, set, "GrossExposure"); got.Status != StatusMeasured {
		t.Errorf("projected GrossExposure status = %q, want %q", got.Status, StatusMeasured)
	}
	if set.QualityFlags == nil {
		t.Error("scenario quality flags is nil rather than empty")
	}
}
