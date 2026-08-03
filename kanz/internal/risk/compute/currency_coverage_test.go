package compute_test

import (
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// The #257 scenario, end to end through the compute layer: a USD-base
// portfolio holding one USD and one EUR position.
//
// Before the fix the EUR position was dropped from GrossExposure,
// NetExposure, VaR99, Delta and HHI, and the returned MeasureSet was
// shaped exactly like one computed over a complete book. These tests
// pin the two halves that make it no longer silent: the numbers are
// still base-currency-only (there is no FX layer, RISK-06 — that is not
// what changed), and the set now says which positions it left out.
func mixedCurrencyBook(t *testing.T) *domain.Portfolio {
	t.Helper()
	return makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "US-A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "EU-B", MarketValue: mkMoney(9000, 0, "EUR"), AsOf: baseTime},
	)
}

func TestComputeMeasures_ReportsCurrencyExcludedPositions(t *testing.T) {
	ms := compute.ComputeMeasures(mixedCurrencyBook(t), nil, nil)

	ex := ms.CurrencyExclusions()
	if len(ex) != 1 {
		t.Fatalf("CurrencyExclusions()=%v, want exactly the EUR position\n\n"+
			"The EUR holding contributes to no measure in this set. If the set does "+
			"not say so, every money measure on it is understated with nothing to "+
			"indicate it — a concentration limit checked against it passes when the "+
			"whole book would breach (#257).", ex)
	}
	if ex[0].InstrumentID != "EU-B" {
		t.Errorf("excluded InstrumentID=%q want %q", ex[0].InstrumentID, "EU-B")
	}
	if ex[0].Currency != "EUR" {
		t.Errorf("excluded Currency=%q want %q — the caller needs to know WHICH "+
			"currency it would need converted, not just that something is missing",
			ex[0].Currency, "EUR")
	}
}

func TestComputeMeasures_ExcludedPositionIsAbsentFromEveryMoneyMeasure(t *testing.T) {
	ms := compute.ComputeMeasures(mixedCurrencyBook(t), nil, nil)

	// The exclusion is still real — the fix reports it, it does not convert.
	// 9000 EUR is 9× the USD leg, so any measure that had silently included
	// it would be unmistakable.
	for _, tc := range []struct {
		name v1.MeasureName
		want int64
	}{
		{compute.MeasureGrossExposure, 1000},
		{compute.MeasureNetExposure, 1000},
	} {
		m, ok := ms.Lookup(tc.name)
		if !ok {
			t.Fatalf("%s missing from the default registry", tc.name)
		}
		if m.Value.GetCoefficient() != tc.want {
			t.Errorf("%s=%d want %d (base-currency only — no FX layer, RISK-06)",
				tc.name, m.Value.GetCoefficient(), tc.want)
		}
	}
}

func TestComputeMeasures_SingleCurrencyBookReportsNoExclusions(t *testing.T) {
	// The other half of "'nothing configured' and 'checked, and fine' must
	// never look the same": a fully measured book must come back clean, or
	// the flag means nothing and callers learn to ignore it.
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "US-A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "US-B", MarketValue: mkMoney(-300, 0, "USD"), AsOf: baseTime},
	)
	if ex := compute.ComputeMeasures(p, nil, nil).CurrencyExclusions(); len(ex) != 0 {
		t.Errorf("CurrencyExclusions()=%v want empty — every position is in base currency", ex)
	}
}

func TestComputeMeasures_UnmarkedPositionIsReportedTooNotJustForeignOnes(t *testing.T) {
	// A nil MarketValue is dropped by the same predicate and is equally
	// absent from the result, so it is equally worth reporting. Currency is
	// "" because there was no Money to read one from.
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "US-A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "NO-MARK", AsOf: baseTime},
	)
	ex := compute.ComputeMeasures(p, nil, nil).CurrencyExclusions()
	if len(ex) != 1 || ex[0].InstrumentID != "NO-MARK" {
		t.Fatalf("CurrencyExclusions()=%v want the unmarked position", ex)
	}
	if ex[0].Currency != "" {
		t.Errorf("Currency=%q want \"\" for a position with no MarketValue", ex[0].Currency)
	}
}

// The exclusion set must be identical to what the filter actually did.
// Portfolio.CurrencyExclusions and Position.InBaseCurrency share one
// predicate precisely so these cannot disagree; this pins that they do
// not, across every position rather than the handful above.
func TestCurrencyExclusions_AgreesWithTheFilterItReports(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(2, 0, "EUR"), AsOf: baseTime},
		domain.Position{InstrumentID: "C", MarketValue: mkMoney(3, 0, "JPY"), AsOf: baseTime},
		domain.Position{InstrumentID: "D", MarketValue: mkMoney(4, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "E", AsOf: baseTime},
	)
	reported := make(map[v1.InstrumentID]bool)
	for _, ex := range p.CurrencyExclusions() {
		reported[ex.InstrumentID] = true
	}
	for _, pos := range p.Positions() {
		dropped := !pos.InBaseCurrency(p.BaseCurrency())
		if dropped != reported[pos.InstrumentID] {
			t.Errorf("position %q: filter drops=%v but reported-excluded=%v — the "+
				"exclusion record has drifted from the exclusions actually performed, "+
				"which is the silent-partial-number bug wearing a flag (#257)",
				pos.InstrumentID, dropped, reported[pos.InstrumentID])
		}
	}
}
