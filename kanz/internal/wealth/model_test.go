package wealth

import (
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/optimization"
)

func modelCatalog() []ModelPortfolio {
	return []ModelPortfolio{
		{ModelID: "M-CONS", Profile: ProfileConservative, Targets: map[string]float64{"VTI": 0.30, "BND": 0.70}},
		{ModelID: "M-BAL", Profile: ProfileBalanced, Targets: map[string]float64{"VTI": 0.60, "BND": 0.40}},
		{ModelID: "M-AGG", Profile: ProfileAggressive, Targets: map[string]float64{"VTI": 0.90, "BND": 0.10}},
	}
}

func TestSelectModel(t *testing.T) {
	m, ok := SelectModel(ProfileBalanced, modelCatalog())
	if !ok || m.ModelID != "M-BAL" {
		t.Fatalf("SelectModel(Balanced) = %q,%v", m.ModelID, ok)
	}
	if _, ok := SelectModel(ProfileGrowth, modelCatalog()); ok {
		t.Errorf("expected no model for Growth")
	}
}

func TestComputeDrift(t *testing.T) {
	// Actual is overweight equity vs the balanced model.
	actual := map[string]float64{"VTI": 0.75, "BND": 0.25}
	model := ModelPortfolio{Targets: map[string]float64{"VTI": 0.60, "BND": 0.40}}
	d := ComputeDrift(actual, model)
	if math.Abs(d.ByInstrument["VTI"]-0.15) > 1e-12 {
		t.Errorf("VTI drift = %v, want 0.15", d.ByInstrument["VTI"])
	}
	if math.Abs(d.ByInstrument["BND"]-(-0.15)) > 1e-12 {
		t.Errorf("BND drift = %v, want -0.15", d.ByInstrument["BND"])
	}
	if math.Abs(d.Max-0.15) > 1e-12 {
		t.Errorf("max drift = %v, want 0.15", d.Max)
	}
	if math.Abs(d.Total-0.30) > 1e-12 {
		t.Errorf("total drift = %v, want 0.30", d.Total)
	}
	if !d.Breached(0.10) {
		t.Errorf("0.15 drift should breach a 0.10 band")
	}
	if d.Breached(0.20) {
		t.Errorf("0.15 drift should not breach a 0.20 band")
	}
}

func TestComputeDrift_CoversModelNotHeld(t *testing.T) {
	// Held only VTI; model wants BND too — BND drifts by its full target.
	d := ComputeDrift(map[string]float64{"VTI": 1.0}, ModelPortfolio{Targets: map[string]float64{"VTI": 0.6, "BND": 0.4}})
	if math.Abs(d.ByInstrument["BND"]-(-0.4)) > 1e-12 {
		t.Errorf("unheld BND drift = %v, want -0.4", d.ByInstrument["BND"])
	}
}

func TestPropose_RespectsRiskProfile(t *testing.T) {
	// A balanced household drifted into an aggressive-looking book.
	h := Household{HouseholdID: "HH1", Accounts: []Account{{
		AccountID: "A1",
		Holdings: []Holding{
			{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 900},
			{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 100},
		},
	}}}
	vp := Aggregate(h)
	prices := map[string]float64{"VTI": 100, "BND": 50}

	prop, ok := Propose(ProfileBalanced, modelCatalog(), vp, prices, 0.01, time.Unix(0, 0))
	if !ok {
		t.Fatal("Propose returned !ok for a profile with a model")
	}
	// The proposal targets the BALANCED model — respecting the risk profile.
	if prop.ModelID != "M-BAL" {
		t.Errorf("proposal model = %q, want M-BAL", prop.ModelID)
	}
	if prop.Profile != ProfileBalanced {
		t.Errorf("proposal profile = %v, want Balanced", prop.Profile)
	}
	// Rebalance targets must equal the balanced model exactly (no asset outside
	// the model, no weight off the model) — the "respects the profile" invariant.
	want := map[string]float64{"VTI": 0.60, "BND": 0.40}
	for id, w := range want {
		if math.Abs(prop.Rebalance.Targets[id]-w) > 1e-12 {
			t.Errorf("target[%s] = %v, want %v", id, prop.Rebalance.Targets[id], w)
		}
	}
	if len(prop.Rebalance.Targets) != len(want) {
		t.Errorf("targets carry %d instruments, want %d", len(prop.Rebalance.Targets), len(want))
	}
	// Drifted 0.90→0.60 equity ⇒ the proposal must SELL VTI / BUY BND.
	var soldVTI, boughtBND bool
	for _, tr := range prop.Rebalance.Trades {
		if tr.InstrumentID == "VTI" && tr.Side == optimization.Sell {
			soldVTI = true
		}
		if tr.InstrumentID == "BND" && tr.Side == optimization.Buy {
			boughtBND = true
		}
	}
	if !soldVTI || !boughtBND {
		t.Errorf("expected SELL VTI + BUY BND, got trades %+v", prop.Rebalance.Trades)
	}
	if !prop.Drift.Breached(0.10) {
		t.Errorf("0.30 equity drift should breach a 0.10 band")
	}
}

func TestPropose_NoModelForProfile(t *testing.T) {
	vp := Aggregate(sampleHousehold())
	if _, ok := Propose(ProfileGrowth, modelCatalog(), vp, nil, 0.01, time.Unix(0, 0)); ok {
		t.Errorf("expected !ok for a profile with no model")
	}
}
