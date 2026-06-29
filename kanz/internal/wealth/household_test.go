package wealth

import (
	"math"
	"testing"
)

func sampleHousehold() Household {
	return Household{
		HouseholdID: "HH1",
		Accounts: []Account{
			{
				AccountID: "A1",
				Cash:      100,
				Holdings: []Holding{
					{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 600},
					{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 300},
				},
			},
			{
				AccountID: "A2",
				Cash:      0,
				Holdings: []Holding{
					{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 400},
					{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 100},
				},
			},
		},
	}
}

func TestAggregate_SumsAcrossAccounts(t *testing.T) {
	vp := Aggregate(sampleHousehold())

	// VTI: 600+400=1000, BND: 300+100=400, cash 100 ⇒ total 1500.
	if vp.Holdings["VTI"] != 1000 {
		t.Errorf("VTI aggregate = %v, want 1000", vp.Holdings["VTI"])
	}
	if vp.Holdings["BND"] != 400 {
		t.Errorf("BND aggregate = %v, want 400", vp.Holdings["BND"])
	}
	if vp.Cash != 100 {
		t.Errorf("cash = %v, want 100", vp.Cash)
	}
	if vp.TotalValue != 1500 {
		t.Errorf("total = %v, want 1500", vp.TotalValue)
	}
}

func TestWeights_ShareOfTotalIncludingCash(t *testing.T) {
	vp := Aggregate(sampleHousehold())
	w := vp.Weights()
	if math.Abs(w["VTI"]-1000.0/1500) > 1e-12 {
		t.Errorf("VTI weight = %v, want %v", w["VTI"], 1000.0/1500)
	}
	if math.Abs(w["BND"]-400.0/1500) > 1e-12 {
		t.Errorf("BND weight = %v", w["BND"])
	}
	// Invested weights sum to 1 − cash share.
	sum := w["VTI"] + w["BND"]
	if math.Abs(sum-(1-100.0/1500)) > 1e-12 {
		t.Errorf("invested weight sum = %v, want %v", sum, 1-100.0/1500)
	}
}

func TestAssetClassExposure_GroupsAndIncludesCash(t *testing.T) {
	vp := Aggregate(sampleHousehold())
	exp := vp.AssetClassExposure()
	if math.Abs(exp["EQUITY"]-1000.0/1500) > 1e-12 {
		t.Errorf("EQUITY exposure = %v", exp["EQUITY"])
	}
	if math.Abs(exp["FIXED_INCOME"]-400.0/1500) > 1e-12 {
		t.Errorf("FIXED_INCOME exposure = %v", exp["FIXED_INCOME"])
	}
	if math.Abs(exp["CASH"]-100.0/1500) > 1e-12 {
		t.Errorf("CASH exposure = %v", exp["CASH"])
	}
	var total float64
	for _, v := range exp {
		total += v
	}
	if math.Abs(total-1) > 1e-12 {
		t.Errorf("exposure sums to %v, want 1", total)
	}
}

func TestAggregate_EmptyHouseholdDegradesToZero(t *testing.T) {
	vp := Aggregate(Household{HouseholdID: "EMPTY"})
	if vp.TotalValue != 0 {
		t.Fatalf("empty total = %v, want 0", vp.TotalValue)
	}
	if len(vp.Weights()) != 0 {
		t.Errorf("empty weights = %v, want empty", vp.Weights())
	}
	if len(vp.AssetClassExposure()) != 0 {
		t.Errorf("empty exposure = %v, want empty", vp.AssetClassExposure())
	}
}
