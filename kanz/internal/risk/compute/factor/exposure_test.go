package factor_test

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
)

var asOf = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func money(amount int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amount, Exponent: 0}, CurrencyCode: ccy}
}

func mval(m *commonpb.Money) float64 {
	if m == nil || m.Amount == nil {
		return 0
	}
	return float64(m.Amount.Coefficient) * math.Pow10(int(m.Amount.Exponent))
}

func pos(inst string, amount int64, ccy string) domain.Position {
	return domain.Position{InstrumentID: v1.InstrumentID(inst), MarketValue: money(amount, ccy)}
}

func portfolio(positions ...domain.Position) *domain.Portfolio {
	p := domain.NewPortfolio(v1.PortfolioID("P1"), domain.CurrencyCode("USD"))
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: domain.CurrencyCode("USD")})
	for _, x := range positions {
		p.SetPosition(x)
	}
	return p
}

// tech/energy GICS classifications; UNKNOWN deliberately absent; VOD has no
// sector (e.g. an ADR with sectorless ref data).
var classifier = factor.StaticClassifier{
	"AAPL": {Sector: factor.Sector{Taxonomy: "GICS", Code: "45"}, AssetClass: "EQUITY"},
	"MSFT": {Sector: factor.Sector{Taxonomy: "GICS", Code: "45"}, AssetClass: "EQUITY"},
	"XOM":  {Sector: factor.Sector{Taxonomy: "GICS", Code: "10"}, AssetClass: "EQUITY"},
	"VOD":  {AssetClass: "EQUITY"}, // known but sectorless
}

func sectorBucket(items []domain.Exposure, key string) (domain.Exposure, bool) {
	for _, e := range items {
		if e.Key == key {
			return e, true
		}
	}
	return domain.Exposure{}, false
}

func TestSectorExposure_BucketsByGICS(t *testing.T) {
	p := portfolio(
		pos("AAPL", 1000, "USD"),
		pos("MSFT", 500, "USD"),
		pos("XOM", -200, "USD"), // short
	)
	items := factor.SectorExposure(context.Background(), p, classifier)

	tech, ok := sectorBucket(items, "GICS:45")
	if !ok {
		t.Fatal("missing GICS:45 bucket")
	}
	if g, n := mval(tech.Gross), mval(tech.Net); g != 1500 || n != 1500 {
		t.Fatalf("tech bucket gross/net = %v/%v, want 1500/1500", g, n)
	}
	energy, ok := sectorBucket(items, "GICS:10")
	if !ok {
		t.Fatal("missing GICS:10 bucket")
	}
	if g, n := mval(energy.Gross), mval(energy.Net); g != 200 || n != -200 {
		t.Fatalf("energy bucket gross/net = %v/%v, want 200/-200", g, n)
	}
	for _, e := range items {
		if e.Dimension != domain.ExposureBySector {
			t.Fatalf("non-sector dimension leaked: %v", e.Dimension)
		}
	}
}

func TestSectorExposure_UnclassifiedCatchAll(t *testing.T) {
	p := portfolio(
		pos("UNKNOWN", 300, "USD"), // not in the classifier
		pos("VOD", 100, "USD"),     // known but sectorless
	)
	items := factor.SectorExposure(context.Background(), p, classifier)
	un, ok := sectorBucket(items, factor.UnclassifiedSector)
	if !ok {
		t.Fatal("missing UNCLASSIFIED bucket")
	}
	if g := mval(un.Gross); g != 400 {
		t.Fatalf("unclassified gross = %v, want 400 (300 unknown + 100 sectorless)", g)
	}
}

// TestSectorExposure_Reconciles: the SECTOR decomposition sums to the same total
// gross/net as the per-instrument exposure (base-currency positions), the
// invariant the UNCLASSIFIED catch-all exists to preserve.
func TestSectorExposure_Reconciles(t *testing.T) {
	p := portfolio(
		pos("AAPL", 1000, "USD"),
		pos("XOM", -200, "USD"),
		pos("UNKNOWN", 300, "USD"),
		pos("VOD", 100, "EUR"), // non-base ⇒ excluded from both sides
	)
	var sumGross, sumNet float64
	for _, e := range factor.SectorExposure(context.Background(), p, classifier) {
		sumGross += mval(e.Gross)
		sumNet += mval(e.Net)
	}
	// base-currency positions: 1000, -200, 300 ⇒ gross 1500, net 1100.
	if sumGross != 1500 || sumNet != 1100 {
		t.Fatalf("sector totals gross/net = %v/%v, want 1500/1100 (reconcile)", sumGross, sumNet)
	}
}

func TestSectorExposure_SkipsNonBaseCurrency(t *testing.T) {
	p := portfolio(pos("AAPL", 1000, "EUR")) // base is USD
	if items := factor.SectorExposure(context.Background(), p, classifier); len(items) != 0 {
		t.Fatalf("non-base positions must be skipped, got %d buckets", len(items))
	}
}

func TestComputeExposure_AddsSectorDimension(t *testing.T) {
	p := portfolio(pos("AAPL", 1000, "USD"))
	set := factor.ComputeExposure(context.Background(), p, classifier)
	got := map[domain.ExposureDimension]bool{}
	for _, e := range set.Items() {
		got[e.Dimension] = true
	}
	for _, d := range []domain.ExposureDimension{
		domain.ExposureByInstrument, domain.ExposureByCurrency, domain.ExposureBySector,
	} {
		if !got[d] {
			t.Errorf("dimension %v missing from combined ExposureSet", d)
		}
	}
}
