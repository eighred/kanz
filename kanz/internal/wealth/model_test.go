package wealth

import (
	"errors"
	"github.com/eighred/kanz/internal/optimization"
	"testing"
	"time"
)

func modelCatalog() []ModelPortfolio {
	return []ModelPortfolio{
		{ModelID: "M-CONS", Profile: ProfileConservative, Targets: map[string]Number{"VTI": "0.3", "BND": "0.7"}, Tolerance: "0.05", RecordedBy: "test:advisor", Reason: "test model"},
		{ModelID: "M-BAL", Profile: ProfileBalanced, Targets: map[string]Number{"VTI": "0.6", "BND": "0.4"}, Tolerance: "0.05", RecordedBy: "test:advisor", Reason: "test model"},
		{ModelID: "M-AGG", Profile: ProfileAggressive, Targets: map[string]Number{"VTI": "0.9", "BND": "0.1"}, Tolerance: "0.05", RecordedBy: "test:advisor", Reason: "test model"},
	}
}
func TestSelectModel(t *testing.T) {
	m, ok := SelectModel(ProfileBalanced, modelCatalog())
	if !ok || m.ModelID != "M-BAL" {
		t.Fatal(m, ok)
	}
	if _, ok := SelectModel(ProfileGrowth, modelCatalog()); ok {
		t.Fatal("missing model selected")
	}
}
func TestComputeDrift(t *testing.T) {
	d, err := ComputeDrift(map[string]Number{"VTI": "0.75", "BND": "0.25"}, modelCatalog()[1])
	if err != nil {
		t.Fatal(err)
	}
	if d.ByInstrument["VTI"] != "0.15" || d.ByInstrument["BND"] != "-0.15" || d.Max != "0.15" || d.Total != "0.3" {
		t.Fatal(d)
	}
	for _, tc := range []struct {
		band Number
		want bool
	}{{"0.1", true}, {"0.15", false}, {"0.2", false}} {
		got, err := d.Breached(tc.band)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}
func TestComputeDrift_CoversModelNotHeld(t *testing.T) {
	d, err := ComputeDrift(map[string]Number{"VTI": "1"}, modelCatalog()[1])
	if err != nil || d.ByInstrument["BND"] != "-0.4" {
		t.Fatal(d, err)
	}
}
func TestPropose_RespectsRiskProfile(t *testing.T) {
	h := Household{HouseholdID: "HH1", Accounts: []Account{{AccountID: "A1", Cash: "0", Holdings: []Holding{{InstrumentID: "VTI", MarketValue: "900"}, {InstrumentID: "BND", MarketValue: "100"}}}}}
	vp := mustAggregate(t, h)
	prices := map[string]Number{"VTI": "100", "BND": "50"}
	p, err := Propose(ProfileBalanced, modelCatalog(), vp, prices, "0.01", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.ModelID != "M-BAL" || p.Profile != ProfileBalanced || p.Rebalance.Targets["VTI"] != "0.6" || p.Rebalance.Targets["BND"] != "0.4" || len(p.Rebalance.Trades) != 2 {
		t.Fatal(p)
	}
	for _, tr := range p.Rebalance.Trades {
		if tr.Notional != "300" {
			t.Fatal(tr)
		}
		if tr.InstrumentID == "VTI" && (tr.Side != optimization.Sell || tr.Quantity != "3") {
			t.Fatal(tr)
		}
		if tr.InstrumentID == "BND" && (tr.Side != optimization.Buy || tr.Quantity != "6") {
			t.Fatal(tr)
		}
	}
	if _, err := Propose(ProfileBalanced, modelCatalog(), vp, nil, "0.01", time.Unix(0, 0)); err == nil {
		t.Fatal("missing prices invented quantities")
	}
}
func TestPropose_NoModelForProfile(t *testing.T) {
	_, err := Propose(ProfileGrowth, modelCatalog(), mustAggregate(t, sampleHousehold()), nil, "0.01", time.Unix(0, 0))
	if !errors.Is(err, ErrNoModelForProfile) {
		t.Fatal(err)
	}
}
func TestDriftUsesExactBandBoundary(t *testing.T) {
	m := modelCatalog()[1]
	d, err := ComputeDrift(map[string]Number{"VTI": "0.650000000000000000001", "BND": "0.349999999999999999999"}, m)
	if err != nil {
		t.Fatal(err)
	}
	breached, err := d.Breached("0.05")
	if err != nil || !breached {
		t.Fatalf("rounded boundary: %+v %v", d, err)
	}
}
