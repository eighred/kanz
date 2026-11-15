package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing"
)

// NOTHING IN THIS ESTATE COMPUTES A GREEK, AND Delta ANSWERS ANYWAY (#1055).
//
// These pin the three things the posture has to get right, each of which this
// repository has already got wrong once somewhere:
//
//   - the series exists before any registry is known (a collector behind a
//     branch exported nothing, #973/#963);
//   - the zero is DERIVED from the registry rather than asserted, so it flips by
//     itself the day RegisterGreeks is wired (#1043's discipline);
//   - a wired family reads 1, so the posture is not a constant wearing a gauge's
//     shape.

// greekSeries returns every kanz_risk_greek_model_live series as measure -> value.
func greekSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_risk_greek_model_live" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "measure" {
					out[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// catalogued greek names, taken from the catalogue rather than typed out — the
// same source the posture reads, so a family that gains a member is covered here
// without anyone remembering.
func cataloguedGreeks() []string {
	var out []string
	for _, m := range compute.Catalogue() {
		if m.Family == compute.FamilyGreeks {
			out = append(out, string(m.Name))
		}
	}
	return out
}

// THE SERIES EXISTS BEFORE THE REGISTRY DOES. NewGreekModelPosture is called at
// the composition root ahead of the broker branch, so a pod that never reaches
// runEngine — the broker-less deployment, or one that dies wiring the spine —
// still exports the whole family. An absent series answers `== 0` with no-data,
// which is the silence this gauge exists to end.
func TestGreekModelPosture_SeedsEveryGreekBeforeAnyRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewGreekModelPosture(reg)

	got := greekSeries(t, reg)
	greeks := cataloguedGreeks()
	if len(greeks) < 5 {
		t.Fatalf("the catalogue lists %d greeks (%v) — fewer than the five RegisterGreeks "+
			"installs, so this test is checking almost nothing", len(greeks), greeks)
	}
	for _, name := range greeks {
		v, ok := got[name]
		if !ok {
			t.Errorf("kanz_risk_greek_model_live{measure=%q} is ABSENT before State runs — a pod "+
				"that never reaches the broker branch reports nothing about a Greek family it "+
				"cannot compute", name)
			continue
		}
		if v != 0 {
			t.Errorf("kanz_risk_greek_model_live{measure=%q} seeds at %v, want 0 — seeding at 1 "+
				"would claim a pricing model this build has no caller for", name, v)
		}
	}
}

// THE DEFECT, AS A SERIES. On the registry every engine actually builds, Delta is
// registered and answers with net exposure while the other four are dark — so the
// family is not model-served and every member must read 0, INCLUDING the one that
// answers. That distinction is the whole point: kanz_risk_measure_live reads 1 for
// Delta here, which is true about the name and says nothing about the number.
func TestGreekModelPosture_TheDefaultRegistryServesNoGreekModel(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewGreekModelPosture(reg)

	var logs bytes.Buffer
	placeholder := p.State(slog.New(slog.NewTextHandler(&logs, nil)), compute.DefaultRegistry())

	got := greekSeries(t, reg)
	for _, name := range cataloguedGreeks() {
		if got[name] != 0 {
			t.Errorf("kanz_risk_greek_model_live{measure=%q} = %v on compute.DefaultRegistry(), "+
				"want 0 — RegisterGreeks has no production caller, so no deployment computes a "+
				"Greek from an option-pricing model (#1055)", name, got[name])
		}
	}
	if len(placeholder) != 1 || placeholder[0] != string(compute.MeasureDelta) {
		t.Errorf("State reports %v served by something other than a pricing model, want [Delta] "+
			"— Delta is the one Greek name the default registry answers, and it answers with "+
			"the portfolio's net exposure", placeholder)
	}

	// AND IT IS SAID OUT LOUD, AT WARN. The gauge is the durable half; the line is
	// what a person reads on the day the pod starts. Info is where it would be
	// filtered out, and "a mandate naming Delta refuses every order" is not an
	// Info-level sentence.
	line := logs.String()
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("the unwired-Greeks posture was not logged at WARN: %s", line)
	}
	if !strings.Contains(line, "Delta") {
		t.Errorf("the posture log does not name Delta, which is the measure that ANSWERS: %s", line)
	}
}

// AND A WIRED FAMILY READS 1. Without this arm the gauge is indistinguishable
// from a constant zero, and the day RegisterGreeks is wired nobody would learn it
// from the signal that exists to say so.
func TestGreekModelPosture_AWiredFamilyReportsItself(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewGreekModelPosture(reg)

	registry := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), registry, compute.GreeksProviders{
		Terms: nil, Spot: nil, Vol: nil, Curve: pricing.FlatCurve(0),
	})

	placeholder := p.State(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), registry)

	got := greekSeries(t, reg)
	for _, name := range cataloguedGreeks() {
		if got[name] != 1 {
			t.Errorf("kanz_risk_greek_model_live{measure=%q} = %v after RegisterGreeks, want 1 — "+
				"the posture is not derived from the registry and would report an unwired "+
				"family forever", name, got[name])
		}
	}
	if len(placeholder) != 0 {
		t.Errorf("State still reports %v as placeholder-served after RegisterGreeks ran", placeholder)
	}
}
