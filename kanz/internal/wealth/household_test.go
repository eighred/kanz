package wealth

import (
	"errors"
	"math/big"
	"testing"
)

func sampleHousehold() Household {
	return Household{
		HouseholdID: "HH1",
		Accounts: []Account{
			{
				AccountID: "A1",
				Cash:      "100",
				Holdings: []Holding{
					{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "600"},
					{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "300"},
				},
			},
			{
				AccountID: "A2",
				Cash:      "0",
				Holdings: []Holding{
					{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "400"},
					{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "100"},
				},
			},
		},
	}
}

func mustAggregate(t *testing.T, h Household) VirtualPortfolio {
	t.Helper()
	v, err := Aggregate(h)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestAggregate_SumsAcrossAccounts(t *testing.T) {
	vp := mustAggregate(t, sampleHousehold())
	if vp.Holdings["VTI"] != "1000" || vp.Holdings["BND"] != "400" || vp.Cash != "100" || vp.TotalValue != "1500" {
		t.Fatalf("wrong exact aggregate: %+v", vp)
	}
}
func TestWeights_ShareOfTotalIncludingCash(t *testing.T) {
	vp := mustAggregate(t, sampleHousehold())
	w, err := vp.Weights()
	if err != nil {
		t.Fatal(err)
	}
	if w["VTI"] != "2/3" || w["BND"] != "4/15" {
		t.Fatalf("wrong exact weights: %v", w)
	}
}
func TestAssetClassExposure_GroupsAndIncludesCash(t *testing.T) {
	vp := mustAggregate(t, sampleHousehold())
	exp, err := vp.AssetClassExposure()
	if err != nil {
		t.Fatal(err)
	}
	if exp["EQUITY"] != "2/3" || exp["FIXED_INCOME"] != "4/15" || exp["CASH"] != "1/15" {
		t.Fatalf("wrong exposure: %v", exp)
	}
	total := new(big.Rat)
	for _, v := range exp {
		r, err := v.Rat()
		if err != nil {
			t.Fatal(err)
		}
		total.Add(total, r)
	}
	if total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatal(total)
	}
}
func TestAggregate_ZeroHasUndefinedWeights(t *testing.T) {
	vp := mustAggregate(t, Household{HouseholdID: "EMPTY"})
	if vp.TotalValue != "0" {
		t.Fatal(vp)
	}
	if _, err := vp.Weights(); !errors.Is(err, ErrWeights) {
		t.Fatalf("zero weight result: %v", err)
	}
	if _, err := vp.AssetClassExposure(); !errors.Is(err, ErrWeights) {
		t.Fatalf("zero exposure result: %v", err)
	}
}
func TestAggregate_PreservesBeyondFloatAndMissing(t *testing.T) {
	h := Household{HouseholdID: "exact", Accounts: []Account{{AccountID: "a", Cash: "9007199254740993.000000001", Holdings: []Holding{{InstrumentID: "x", MarketValue: "0.000000002"}}}}}
	v := mustAggregate(t, h)
	if v.TotalValue != "9007199254740993.000000003" {
		t.Fatal(v.TotalValue)
	}
	h.Accounts[0].Cash = ""
	if _, err := Aggregate(h); err == nil {
		t.Fatal("missing cash became zero")
	}
}
