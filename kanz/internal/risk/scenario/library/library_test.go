package library_test

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	"github.com/eighred/kanz/internal/risk/scenario/library"
)

func TestNamed_LookupAndUnknown(t *testing.T) {
	if _, ok := library.Named("GFC_2008"); !ok {
		t.Error("GFC_2008 should be in the catalog")
	}
	if _, ok := library.Named("NOPE"); ok {
		t.Error("unknown scenario should miss")
	}
}

func TestNames_StableAndComplete(t *testing.T) {
	names := library.Names()
	want := []string{"COVID_2020", "GFC_2008"} // sorted
	if len(names) != len(want) {
		t.Fatalf("names=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d]=%q want %q", i, names[i], want[i])
		}
	}
}

func TestNamedScenarios_AreFullGICSCurves(t *testing.T) {
	// Each named scenario shocks all eleven GICS sectors, deterministically
	// ordered by code, and only via SectorShock.
	for _, name := range library.Names() {
		shocks, _ := library.Named(name)
		if len(shocks) != 11 {
			t.Errorf("%s: %d shocks want 11 GICS sectors", name, len(shocks))
		}
		var prev string
		for _, s := range shocks {
			ss, ok := s.(v1.SectorShock)
			if !ok {
				t.Fatalf("%s: shock %T is not a SectorShock", name, s)
			}
			if ss.Taxonomy != "GICS" {
				t.Errorf("%s: taxonomy %q want GICS", name, ss.Taxonomy)
			}
			if ss.Code <= prev {
				t.Errorf("%s: codes not strictly ascending at %q (prev %q)", name, ss.Code, prev)
			}
			prev = ss.Code
		}
	}
}

func TestSectorCurve_DropsNilAndZeroPoints(t *testing.T) {
	curve := library.SectorCurve("GICS", map[string]*commonpb.Decimal{
		"40": {Coefficient: -50, Exponent: -2},
		"45": nil,                            // dropped — no shift
		"10": {Coefficient: 0, Exponent: -2}, // dropped — zero shift
	})
	if len(curve) != 1 {
		t.Fatalf("curve=%v want 1 effective shock", curve)
	}
	if ss := curve[0].(v1.SectorShock); ss.Code != "40" {
		t.Errorf("kept code %q want 40", ss.Code)
	}
}

// End-to-end: a named scenario drives the scenario engine over a classified
// book and shocks each position by its sector's curve point.
func TestGFC2008_AppliedDifferentiatesBySector(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("PORT-1", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: base, BaseCurrency: "USD"})
	p.SetPosition(domain.Position{InstrumentID: "BANK", MarketValue: mkMoney(1000), AsOf: base}) // financials 40
	p.SetPosition(domain.Position{InstrumentID: "FOOD", MarketValue: mkMoney(1000), AsOf: base}) // staples 30

	c := factor.StaticClassifier{
		"BANK": {Sector: factor.Sector{Taxonomy: "GICS", Code: "40"}},
		"FOOD": {Sector: factor.Sector{Taxonomy: "GICS", Code: "30"}},
	}
	got, cov := scenario.Evaluate(p, library.GlobalFinancialCrisis2008(), nil, scenario.WithClassifier(c))
	if cov.ExcludedCount != 0 {
		t.Fatalf("coverage=%+v want empty — both holdings are classified, so the curve applies in full", cov)
	}

	// Financials −55% ⇒ 450; staples −15% ⇒ 850; net 1300.
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := toFloat(m.Value); v != 1300 {
		t.Errorf("NetExposure=%v want 1300 (financials hit harder than staples)", v)
	}
}

func mkMoney(amount int64) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amount}, CurrencyCode: "USD"}
}

func toFloat(d *commonpb.Decimal) float64 {
	f := float64(d.Coefficient)
	for e := d.Exponent; e < 0; e++ {
		f /= 10
	}
	for e := d.Exponent; e > 0; e-- {
		f *= 10
	}
	return f
}
