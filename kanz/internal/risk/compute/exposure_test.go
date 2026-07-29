package compute_test

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func mkMoney(coef int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: coef, Exponent: exp},
		CurrencyCode: ccy,
	}
}

// makePortfolio is a test helper that hands every position straight
// through Portfolio.SetPosition — exercises the same write path
// RISK-05 uses.
func makePortfolio(id v1.PortfolioID, ccy domain.CurrencyCode, positions ...domain.Position) *domain.Portfolio {
	p := domain.NewPortfolio(id, ccy)
	p.SetAggregate(domain.AggregateUpdate{
		AsOf:         baseTime,
		BaseCurrency: ccy,
	})
	for _, pos := range positions {
		p.SetPosition(pos)
	}
	return p
}

func TestComputeExposure_EmptyPortfolioYieldsEmptySet(t *testing.T) {
	p := domain.NewPortfolio("PORT-1", "USD")
	es := compute.ComputeExposure(p)
	if got := len(es.Items()); got != 0 {
		t.Errorf("items=%d want 0", got)
	}
	if es.PortfolioID() != "PORT-1" {
		t.Errorf("PortfolioID=%q", es.PortfolioID())
	}
}

func TestComputeExposure_SinglePositionEmitsBothDimensions(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID: "AAPL",
			MarketValue:  mkMoney(1500, 0, "USD"),
			AsOf:         baseTime,
		},
	)
	es := compute.ComputeExposure(p)
	items := es.Items()
	if got := len(items); got != 2 {
		t.Fatalf("items=%d want 2 (instrument + currency)", got)
	}
	var instr, ccy *domain.Exposure
	for i := range items {
		switch items[i].Dimension {
		case domain.ExposureByInstrument:
			instr = &items[i]
		case domain.ExposureByCurrency:
			ccy = &items[i]
		}
	}
	if instr == nil || instr.Key != "AAPL" {
		t.Fatalf("instrument exposure missing or wrong key: %v", instr)
	}
	if instr.Net.Amount.Coefficient != 1500 {
		t.Errorf("instrument Net=%d want 1500", instr.Net.Amount.Coefficient)
	}
	if ccy == nil || ccy.Key != "USD" {
		t.Fatalf("currency exposure missing: %v", ccy)
	}
	if ccy.Net.Amount.Coefficient != 1500 {
		t.Errorf("currency Net=%d want 1500", ccy.Net.Amount.Coefficient)
	}
}

func TestComputeExposure_ShortPositionGrossIsAbsolute(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID: "SHORT",
			MarketValue:  mkMoney(-800, 0, "USD"), // short position
			AsOf:         baseTime,
		},
	)
	es := compute.ComputeExposure(p)
	for _, e := range es.Items() {
		if e.Dimension != domain.ExposureByInstrument {
			continue
		}
		if e.Net.Amount.Coefficient != -800 {
			t.Errorf("Net=%d want -800 (signed)", e.Net.Amount.Coefficient)
		}
		if e.Gross.Amount.Coefficient != 800 {
			t.Errorf("Gross=%d want 800 (abs)", e.Gross.Amount.Coefficient)
		}
	}
}

func TestComputeExposure_SameCurrencyAggregates(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(500, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "C", MarketValue: mkMoney(-300, 0, "USD"), AsOf: baseTime},
	)
	es := compute.ComputeExposure(p)
	var ccy *domain.Exposure
	for _, e := range es.ByDimension(domain.ExposureByCurrency) {
		if e.Key == "USD" {
			ec := e
			ccy = &ec
			break
		}
	}
	if ccy == nil {
		t.Fatal("USD bucket missing")
	}
	// Net = 1000 + 500 + (-300) = 1200
	if ccy.Net.Amount.Coefficient != 1200 {
		t.Errorf("USD Net=%d want 1200", ccy.Net.Amount.Coefficient)
	}
	// Gross = 1000 + 500 + 300 = 1800
	if ccy.Gross.Amount.Coefficient != 1800 {
		t.Errorf("USD Gross=%d want 1800", ccy.Gross.Amount.Coefficient)
	}
}

func TestComputeExposure_MixedCurrenciesProduceSeparateBuckets(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "AAPL", MarketValue: mkMoney(1500, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "ASML", MarketValue: mkMoney(800, 0, "EUR"), AsOf: baseTime},
		domain.Position{InstrumentID: "TSM", MarketValue: mkMoney(200000, 0, "TWD"), AsOf: baseTime},
	)
	es := compute.ComputeExposure(p)
	buckets := es.ByDimension(domain.ExposureByCurrency)
	if got := len(buckets); got != 3 {
		t.Fatalf("currency buckets=%d want 3", got)
	}
	seen := map[string]int64{}
	for _, b := range buckets {
		seen[b.Key] = b.Net.Amount.Coefficient
	}
	if seen["USD"] != 1500 || seen["EUR"] != 800 || seen["TWD"] != 200000 {
		t.Errorf("bucket Nets: %v", seen)
	}
}

func TestComputeExposure_PositionWithNilMarketValueIsSkipped(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "AAPL", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "UNMARKED", MarketValue: nil, AsOf: baseTime},
	)
	es := compute.ComputeExposure(p)
	for _, e := range es.ByDimension(domain.ExposureByInstrument) {
		if e.Key == "UNMARKED" {
			t.Error("UNMARKED position emitted despite nil MarketValue")
		}
	}
}

func TestComputeExposure_DifferentExponentsSumCorrectly(t *testing.T) {
	// 1.50 + 0.025 = 1.525 — exercises the addDecimal alignment path.
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(150, -2, "USD"), AsOf: baseTime}, // 1.50
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(25, -3, "USD"), AsOf: baseTime},  // 0.025
	)
	es := compute.ComputeExposure(p)
	for _, e := range es.ByDimension(domain.ExposureByCurrency) {
		if e.Key != "USD" {
			continue
		}
		// 1.525 = 1525 × 10^-3
		if e.Net.Amount.Coefficient != 1525 || e.Net.Amount.Exponent != -3 {
			t.Errorf("Net=%d × 10^%d want 1525 × 10^-3", e.Net.Amount.Coefficient, e.Net.Amount.Exponent)
		}
	}
}

func TestComputeExposure_AsOfPropagated(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(100, 0, "USD"), AsOf: baseTime},
	)
	es := compute.ComputeExposure(p)
	if !es.AsOf().Equal(baseTime) {
		t.Errorf("AsOf=%v want %v", es.AsOf(), baseTime)
	}
}
