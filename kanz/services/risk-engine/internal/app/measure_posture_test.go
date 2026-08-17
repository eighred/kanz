package app

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// #509's posture. The interesting case is the one the estate is actually in —
// a registry serving a minority of the catalogue while every client call
// succeeds.

func measurePosture(t *testing.T, r *compute.Registry) (*prometheus.Registry, string) {
	t.Helper()
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	MeasurePosture(reg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), r)
	return reg, logs.String()
}

// seriesFor returns the gauge value for one measure, and whether a series
// exists at all. The second half matters as much as the first: a metric family
// with no series for a measure reads as "no data" to every consumer, so an
// alert on the dark ones could never fire.
func seriesFor(t *testing.T, reg *prometheus.Registry, measure string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_risk_measure_live" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "measure" && l.GetValue() == measure {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// EVERY CATALOGUED MEASURE GETS A SERIES, INCLUDING THE DARK ONES.
//
// This is the whole point. A gauge that emitted only the registered measures
// would climb from nothing to nothing and always read as full coverage.
func TestMeasurePosture_EveryCataloguedMeasureHasASeries(t *testing.T) {
	reg, _ := measurePosture(t, compute.DefaultRegistry())

	if got, want := testutil.CollectAndCount(reg, "kanz_risk_measure_live"), len(compute.Catalogue()); got != want {
		t.Errorf("kanz_risk_measure_live has %d series, want %d — one per catalogued measure. "+
			"Emitting only the live ones is how 18 unregistered measures stayed invisible", got, want)
	}
	for _, m := range compute.Catalogue() {
		if _, ok := seriesFor(t, reg, string(m.Name)); !ok {
			t.Errorf("no series for %q — a measure with no series reads as no-data, so an alert "+
				"on it can never fire", m.Name)
		}
	}
}

// A DARK MEASURE READS ZERO AND THE WARNING NAMES THE CONSEQUENCE.
func TestMeasurePosture_DarkMeasuresAreZeroAndWarned(t *testing.T) {
	reg, logs := measurePosture(t, compute.DefaultRegistry())

	// DV01 is implemented (RegisterFIRisk) and DefaultRegistry does not serve it.
	if got, ok := seriesFor(t, reg, "DV01"); !ok || got != 0 {
		t.Errorf("DV01 = %v (present=%v), want 0 — DefaultRegistry does not register the FI "+
			"measures", got, ok)
	}
	// GrossExposure is in DefaultRegistry.
	if got, ok := seriesFor(t, reg, "GrossExposure"); !ok || got != 1 {
		t.Errorf("GrossExposure = %v (present=%v), want 1", got, ok)
	}

	if !bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Errorf("a partially-served catalogue logged no warning:\n%s", logs)
	}
	// THE MESSAGE MUST NAME THE MECHANISM. An operator who reads "18 measures
	// unregistered" concludes they are optional extras rather than answers that
	// go missing from a successful response.
	for _, want := range []string{"SUCCESSFUL response", "drops unknown names", "no probe fails"} {
		if !bytes.Contains([]byte(logs), []byte(want)) {
			t.Errorf("the warning does not mention %q — it reads as a feature list rather than a "+
				"silent-absence posture:\n%s", want, logs)
		}
	}
}

// A FULLY-SERVED CATALOGUE REPORTS ONES AND DOES NOT WARN.
//
// Without this the test above is satisfied by a function that warns
// unconditionally, and the warning would stop meaning anything.
func TestMeasurePosture_AFullCatalogueDoesNotWarn(t *testing.T) {
	r := compute.NewRegistry()
	for _, m := range compute.Catalogue() {
		r.Register(m.Name, compute.GrossExposure) // any MeasureFunc; presence is what is graded
	}
	reg, logs := measurePosture(t, r)

	for _, m := range compute.Catalogue() {
		if got, _ := seriesFor(t, reg, string(m.Name)); got != 1 {
			t.Errorf("%s = %v with everything registered, want 1", m.Name, got)
		}
	}
	if bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Errorf("a fully-served catalogue warned:\n%s", logs)
	}
}

// stubReturns is the provider varmodel.Register needs. It returns nothing,
// because this test grades WHICH MEASURES ARE REGISTERED, not what they compute.
type stubReturns struct{}

func (stubReturns) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	return nil, nil
}

// THE REAL COMPOSITION SHAPE: what risk-engine's main actually builds.
//
// The composition root is the estate's untested blind spot — wiring escapes
// every unit test, and this platform has twice shipped a crash with a green
// suite. So this reproduces main's two registration calls exactly and pins the
// resulting posture, which is the number #509 is about.
//
// IT ASSERTS A GAP RATHER THAN A FIX. When a seam is wired the count moves and
// this test fails, which is the intended signal: the assertion is deliberately
// written so that PROGRESS breaks it and the failure message says so.
func TestMeasurePosture_TheShapeRiskEngineActuallyBuilds(t *testing.T) {
	registry := compute.DefaultRegistry()
	varmodel.Register(context.Background(), registry, stubReturns{}, varmodel.Config{})
	// Factor needs only the returns provider, so main registers it on the
	// market-data gate alone — unlike FI, which additionally needs a curve.
	compute.RegisterFactorRisk(context.Background(), registry,
		compute.FactorProviders{Model: stubModel{}})

	dark := compute.Dark(registry)
	live := len(compute.Catalogue()) - len(dark)

	if live != 11 {
		t.Errorf("risk-engine's registry serves %d catalogued measures without calibration, not 11.\n"+
			"If a seam was WIRED, update this number and delete the matching entry from "+
			"test/arch/no_dark_measure_seam_test.go's darkSeamExempt. If a measure was added "+
			"without registering it, that is the gap #509 tracks and the count is correct.", live)
	}

	// AND THE DARK ONES ARE WHOLE FAMILIES, not a scattering. That is what makes
	// this four missing wirings rather than eighteen missing measures, and it is
	// the fact the family label exists to carry.
	byFamily := map[compute.MeasureFamily]int{}
	for _, m := range dark {
		byFamily[m.Family]++
	}
	for _, f := range []compute.MeasureFamily{
		compute.FamilyGreeks,
		compute.FamilyLiquidity, compute.FamilyStructured, compute.FamilyXVA,
	} {
		if byFamily[f] == 0 {
			t.Errorf("family %q reports no dark measures — either it was wired (good; update this "+
				"list) or the catalogue lost its entries (bad)", f)
		}
	}
	// The two families that ARE served must be fully served, or the gap is not
	// the clean family split this reports.
	for _, f := range []compute.MeasureFamily{
		compute.FamilyExposure, compute.FamilyTailRisk, compute.FamilyFactor,
	} {
		if byFamily[f] != 0 {
			t.Errorf("family %q has %d dark measures — it is registered by a seam main DOES call, "+
				"so a gap here is a measure that was added and never registered", f, byFamily[f])
		}
	}
}

// stubBondTerms resolves nothing. The FI registration's SHAPE is what this test
// grades — which measures exist — not what they compute.
type stubBondTerms struct{}

func (stubBondTerms) BondTerms(context.Context, string, time.Time) (compute.BondSpec, compute.TermsResolution) {
	return compute.BondSpec{}, compute.TermsUnknown
}

type stubCurve struct{}

func (stubCurve) Curve(context.Context, string, time.Time) (*curve.Curve, bool) { return nil, false }

// WITH CALIBRATION ON, THE FIXED-INCOME FAMILY GOES LIVE (#509).
//
// main registers the FI measures only when rate calibration is enabled, because
// registering them against an empty curve store would serve a DV01 of zero for
// every portfolio — indistinguishable from a book holding no bonds. This
// reproduces that second shape, so the conditional is pinned rather than only the
// default branch a test happens to take.
func TestMeasurePosture_WithCalibrationTheFixedIncomeFamilyIsServed(t *testing.T) {
	registry := compute.DefaultRegistry()
	varmodel.Register(context.Background(), registry, stubReturns{}, varmodel.Config{})
	compute.RegisterFactorRisk(context.Background(), registry,
		compute.FactorProviders{Model: stubModel{}})
	compute.RegisterFIRisk(context.Background(), registry, compute.FIProviders{
		Terms: stubBondTerms{}, Curve: stubCurve{},
	})

	dark := compute.Dark(registry)
	live := len(compute.Catalogue()) - len(dark)
	if live != 15 {
		t.Errorf("with FI registered the engine serves %d measures, not 15 — the four FI "+
			"measures are DV01, Duration, Convexity and SpreadDuration", live)
	}
	for _, m := range dark {
		if m.Family == compute.FamilyFixedIncome {
			t.Errorf("%s is still dark with RegisterFIRisk called", m.Name)
		}
	}

	reg, logs := measurePosture(t, registry)
	for _, name := range []string{"DV01", "Duration", "Convexity", "SpreadDuration"} {
		if got, ok := seriesFor(t, reg, name); !ok || got != 1 {
			t.Errorf("%s = %v (present=%v), want 1", name, got, ok)
		}
	}
	// STILL WARNS, because five families remain dark. A posture that went quiet
	// on the first family being wired would stop reporting the rest.
	if !bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Error("the posture stopped warning while Greeks, factor, liquidity, structured and " +
			"XVA are all still unregistered")
	}
}

// stubModel resolves no model. What these tests grade is which measures EXIST,
// not what they compute.
type stubModel struct{}

func (stubModel) Model(context.Context, time.Time) (*factormodel.Model, bool) { return nil, false }

// THE PLACEHOLDER DELTA READS AS LIVE, AND THAT IS A PROPERTY WORTH PINNING.
//
// compute.Dark asks the registry whether a NAME is registered; it cannot ask
// which implementation registered it. DefaultRegistry registers Delta as the
// RISK-07 net-exposure placeholder, so kanz_risk_measure_live{measure="Delta",
// family="greeks"} is 1 on an engine that serves no pricing-derived Greek.
//
// The catalogue's comment used to assert the opposite — "a served placeholder
// Delta does not make the Greeks family look live" — which was simply false, and
// nothing checked it. This test is what stops that claim being made again: it
// asserts the ACTUAL behaviour, so anyone who changes it has to change this and
// read why.
func TestMeasurePosture_ThePlaceholderDeltaReadsAsLive(t *testing.T) {
	registry := compute.DefaultRegistry()
	reg, _ := measurePosture(t, registry)

	if got, ok := seriesFor(t, reg, "Delta"); !ok || got != 1 {
		t.Errorf("Delta = %v (present=%v), want 1 — DefaultRegistry registers the placeholder, "+
			"and the posture cannot see that it is one", got, ok)
	}
	// AND ITS FAMILY IS STILL MOSTLY DARK, which is the reading that matters. An
	// operator must take family coverage from the other four, not from Delta.
	for _, name := range []string{"Gamma", "Vega", "Theta", "Rho"} {
		if got, ok := seriesFor(t, reg, name); !ok || got != 0 {
			t.Errorf("%s = %v (present=%v), want 0 — no pricing-derived Greek is registered",
				name, got, ok)
		}
	}
}
