package drift

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/wealth"
)

func model() wealth.ModelPortfolio {
	return wealth.ModelPortfolio{
		ModelID:    "growth-2026",
		Profile:    wealth.ProfileGrowth,
		Targets:    map[string]float64{"BTC-USD": 0.6, "ETH-USD": 0.4},
		Tolerance:  0.05,
		RecordedBy: "operator:akif",
		Reason:     "IC review",
	}
}

// household builds a one-account household holding btc and eth at the given
// market values, on the given profile.
func household(profile wealth.RiskProfile, btc, eth float64) wealth.Household {
	return wealth.Household{
		HouseholdID: "hh-1",
		RiskProfile: profile,
		Accounts: []wealth.Account{{
			AccountID: "acct-1",
			Holdings: []wealth.Holding{
				{InstrumentID: "BTC-USD", AssetClass: "CRYPTO", MarketValue: btc},
				{InstrumentID: "ETH-USD", AssetClass: "CRYPTO", MarketValue: eth},
			},
		}},
	}
}

func armedCatalogue(t *testing.T) *wealth.ModelRegistry {
	t.Helper()
	r := wealth.NewModelRegistry()
	if err := r.Put("acme", model()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r.Arm()
	return r
}

// A BOOK ON ITS MODEL IS IN BAND; A BOOK OFF IT IS BREACHED. The numbers are
// computed independently of the code under test: 60/40 is the model, so a
// 6000/4000 book has zero drift, and a 8000/2000 book is 0.80 vs 0.60 = +0.20 on
// BTC — four times the 0.05 band.
func TestEvaluateMeasuresTheBookAgainstItsModel(t *testing.T) {
	m := New("acme", armedCatalogue(t), nil, nil)

	onModel := m.Evaluate(household(wealth.ProfileGrowth, 6000, 4000))
	if onModel.Outcome != OutcomeInBand || !onModel.Evaluated {
		t.Fatalf("a book sitting exactly on its model reported %+v", onModel)
	}
	if onModel.Drift.Max > 1e-9 {
		t.Errorf("max drift = %v on a book that matches its model exactly", onModel.Drift.Max)
	}
	if onModel.ModelID != "growth-2026" || onModel.Tolerance != 0.05 {
		t.Errorf("the result does not carry the model it was measured against: %+v", onModel)
	}

	drifted := m.Evaluate(household(wealth.ProfileGrowth, 8000, 2000))
	if drifted.Outcome != OutcomeBreached || !drifted.Breached {
		t.Fatalf("a book 20 points off a 5-point band reported %+v — nothing would ever flag a "+
			"rebalance", drifted)
	}
	// 0.8 - 0.6 = 0.2 exactly; float64 over these values is exact enough to compare
	// against a tolerance far tighter than the band.
	if got := drifted.Drift.Max; got < 0.199 || got > 0.201 {
		t.Errorf("max drift = %v, want 0.20 (BTC at 0.80 against a 0.60 target)", got)
	}
}

// THE THREE UNEVALUATED STATES CARRY THREE DIFFERENT LABELS, and none of them is
// in_band. This is the rule the whole capability was missing: "not measured" and
// "measured, and fine" must never read the same.
func TestUnevaluableHouseholdsAreNeverReportedAsInBand(t *testing.T) {
	armed := armedCatalogue(t)

	cases := []struct {
		name    string
		monitor *Monitor
		h       wealth.Household
		want    Outcome
	}{
		{"catalogue still replaying", New("acme", wealth.NewModelRegistry(), nil, nil),
			household(wealth.ProfileGrowth, 6000, 4000), OutcomeCatalogueUnarmed},
		{"household asserts no profile", New("acme", armed, nil, nil),
			household(wealth.ProfileUnspecified, 6000, 4000), OutcomeNoProfile},
		{"no model for this profile", New("acme", armed, nil, nil),
			household(wealth.ProfileConservative, 6000, 4000), OutcomeNoModel},
		{"another tenant's catalogue", New("zenith", armed, nil, nil),
			household(wealth.ProfileGrowth, 6000, 4000), OutcomeNoModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.monitor.Evaluate(tc.h)
			if res.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", res.Outcome, tc.want)
			}
			if res.Evaluated {
				t.Error("Evaluated is true on a household that was not measured — a caller reading " +
					"Drift.Max off this result would read 0 as 'matches its model exactly'")
			}
			if res.Drift.Max != 0 || res.ModelID != "" {
				t.Errorf("an unevaluated result carries measurements: %+v", res)
			}
			if res.Reason == "" {
				t.Error("no reason on an unevaluated household — an operator reading the read " +
					"surface has nothing to act on")
			}
		})
	}
}

// AN AMBIGUOUS CATALOGUE IS ITS OWN LABEL, not "no model". They are different
// operator actions: publish targets, versus purge a retired model's subject.
func TestAnAmbiguousProfileHasItsOwnOutcome(t *testing.T) {
	r := wealth.NewModelRegistry()
	first, second := model(), model()
	second.ModelID = "growth-2025"
	for _, m := range []wealth.ModelPortfolio{first, second} {
		if err := r.Put("acme", m); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	r.Arm()

	res := New("acme", r, nil, nil).Evaluate(household(wealth.ProfileGrowth, 6000, 4000))
	if res.Outcome != OutcomeAmbiguousModel {
		t.Fatalf("outcome = %q, want %q — with two models resident one of them silently became the "+
			"target for every household on the profile", res.Outcome, OutcomeAmbiguousModel)
	}
	if !strings.Contains(res.Reason, "growth-2025") {
		t.Errorf("the reason does not name the models in conflict: %q", res.Reason)
	}
}

// EVERY OUTCOME SERIES EXISTS FROM THE FIRST SCRAPE, at zero. A CounterVec label
// that has never been incremented exports NO series, and an alert written over a
// missing series is silent in exactly the state it was written to detect — which
// this repository shipped twice (#963, #973).
func TestEveryOutcomeSeriesExistsBeforeAnythingIsEvaluated(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg, armedCatalogue(t))

	got := counterSeries(t, reg, "kanz_wealth_drift_evaluations_total")
	for _, o := range outcomes {
		if _, ok := got[string(o)]; !ok {
			t.Errorf("no series for outcome=%q before any evaluation. An alert over it is silent "+
				"in exactly the state it exists to detect", o)
		}
	}
	if len(got) != len(outcomes) {
		t.Errorf("exported %d outcome series, want %d — the enumerated list and the constants have "+
			"drifted apart", len(got), len(outcomes))
	}
	for _, name := range []string{"kanz_wealth_model_catalogue_armed", "kanz_wealth_models_registered"} {
		if !gaugeExists(t, reg, name) {
			t.Errorf("%s is not registered. A deployment with no broker would export nothing at all "+
				"rather than exporting 0, and 'not wired' would be invisible", name)
		}
	}
}

// OBSERVE RECORDS; EVALUATE DOES NOT. The read surface calls Evaluate, so a
// dashboard refresh must not move the counters that measure how the BOOK behaves —
// otherwise "how many households drifted this hour" counts page loads.
func TestObserveRecordsAndEvaluateDoesNot(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("acme", armedCatalogue(t), NewMetrics(reg, armedCatalogue(t)), nil)
	drifted := household(wealth.ProfileGrowth, 8000, 2000)

	m.Evaluate(drifted)
	m.Evaluate(drifted)
	if got := counterSeries(t, reg, "kanz_wealth_drift_evaluations_total")[string(OutcomeBreached)]; got != 0 {
		t.Errorf("breached counter = %v after two READS. A query is not a rebalance signal", got)
	}

	m.Observe(drifted)
	if got := counterSeries(t, reg, "kanz_wealth_drift_evaluations_total")[string(OutcomeBreached)]; got != 1 {
		t.Errorf("breached counter = %v after one fold, want 1 — a drifted household moved nothing "+
			"an alert can see", got)
	}
	if got := gaugeValue(t, reg, "kanz_wealth_models_registered"); got != 1 {
		t.Errorf("kanz_wealth_models_registered = %v, want 1", got)
	}
	if got := gaugeValue(t, reg, "kanz_wealth_model_catalogue_armed"); got != 1 {
		t.Errorf("kanz_wealth_model_catalogue_armed = %v, want 1", got)
	}
}

// AN UNEVALUABLE HOUSEHOLD IS COUNTED SEPARATELY RATHER THAN SILENTLY SKIPPED. If
// Observe returned early on a household with no model, a firm that published no
// targets would export the same zeros as a firm whose every book is in band.
func TestObserveCountsAnUnevaluableHousehold(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := func() *Monitor {
		unarmed := wealth.NewModelRegistry()
		return New("acme", unarmed, NewMetrics(reg, unarmed), nil)
	}()

	m.Observe(household(wealth.ProfileGrowth, 6000, 4000))

	series := counterSeries(t, reg, "kanz_wealth_drift_evaluations_total")
	if series[string(OutcomeCatalogueUnarmed)] != 1 {
		t.Errorf("catalogue_unarmed = %v, want 1", series[string(OutcomeCatalogueUnarmed)])
	}
	if series[string(OutcomeInBand)] != 0 {
		t.Errorf("in_band = %v — a household nothing measured was counted as measured and fine",
			series[string(OutcomeInBand)])
	}
	if got := gaugeValue(t, reg, "kanz_wealth_model_catalogue_armed"); got != 0 {
		t.Errorf("kanz_wealth_model_catalogue_armed = %v while the replay has not landed", got)
	}
}

func counterSeries(t *testing.T, g prometheus.Gatherer, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[labelValue(m, "outcome")] = m.GetCounter().GetValue()
		}
	}
	return out
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func gaugeExists(t *testing.T, g prometheus.Gatherer, name string) bool {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return true
		}
	}
	return false
}

func gaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) > 0 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s not exported", name)
	return 0
}

// THE POSTURE GAUGES DO NOT WAIT FOR TRAFFIC. Found by running the binary, not by
// a test: the first version Set them inside Observe, so a service whose catalogue
// had armed correctly but had not yet folded a valuation reported
// kanz_wealth_model_catalogue_armed=0 — "the replay never landed" — for as long as
// no household was revalued. Household valuations are operator-published and can
// be weeks apart, so that is not a momentary window.
//
// An alert on `kanz_wealth_model_catalogue_armed == 0` would have fired against a
// perfectly armed service, and — worse in the other direction — the gauge would
// have been indistinguishable from a genuinely unarmed one.
func TestPostureGaugesReadTheCatalogueNotTheTraffic(t *testing.T) {
	reg := prometheus.NewRegistry()
	cat := armedCatalogue(t)
	NewMetrics(reg, cat)

	// No Observe, no Evaluate — nothing has been folded at all.
	if got := gaugeValue(t, reg, "kanz_wealth_model_catalogue_armed"); got != 1 {
		t.Errorf("kanz_wealth_model_catalogue_armed = %v on an ARMED catalogue that has not yet seen "+
			"a valuation. A posture gauge must be a function of the posture, not of traffic — "+
			"otherwise a correctly armed service is indistinguishable from one whose replay never "+
			"landed", got)
	}
	if got := gaugeValue(t, reg, "kanz_wealth_models_registered"); got != 1 {
		t.Errorf("kanz_wealth_models_registered = %v before any valuation, want 1", got)
	}

	// And it tracks a catalogue that changes after registration, which a Set-once
	// gauge could not do either.
	second := model()
	second.ModelID, second.Profile = "balanced-2026", wealth.ProfileBalanced
	if err := cat.Put("acme", second); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := gaugeValue(t, reg, "kanz_wealth_models_registered"); got != 2 {
		t.Errorf("kanz_wealth_models_registered = %v after a second model landed, want 2", got)
	}
}
