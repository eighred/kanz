package sustainability

import (
	"context"
	"errors"
	"testing"
)

type fakeESGSource struct {
	rows []ESGRow
	err  error
}

func (f fakeESGSource) Vendor() string                          { return "MSCI" }
func (f fakeESGSource) Fetch(context.Context) ([]ESGRow, error) { return f.rows, f.err }

func sampleRows() []ESGRow {
	return []ESGRow{
		{InstrumentID: "AAPL", Overall: 80, Environmental: 75, Social: 82, Governance: 84, Scope1: 100, Scope2: 200, Scope3: 5000, Revenue: 400000, EVIC: 3_000_000},
		{InstrumentID: "XOM", Overall: 40, Environmental: 30, Scope1: 50000, Scope2: 20000, Scope3: 600000, Revenue: 350000, EVIC: 0}, // no EVIC ⇒ coverage gap
		{InstrumentID: "", Overall: 50}, // no id ⇒ skipped
	}
}

func TestIngest_NormalizesAndIndexes(t *testing.T) {
	idx, err := Ingest(context.Background(), fakeESGSource{rows: sampleRows()})
	if err != nil {
		t.Fatal(err)
	}
	aapl, ok := idx.Lookup("AAPL")
	if !ok || aapl.ESG.Overall != 80 || aapl.Carbon.TotalScopes() != 5300 {
		t.Fatalf("AAPL = %+v ok=%v", aapl, ok)
	}
	if _, ok := idx.Lookup(""); ok {
		t.Error("empty id should not be indexed")
	}
}

func TestIngest_CoverageSurfacesNoEVICGap(t *testing.T) {
	idx, _ := Ingest(context.Background(), fakeESGSource{rows: sampleRows()})
	cov := idx.Coverage()
	if cov.Total != 2 || cov.WithEVIC != 1 {
		t.Fatalf("coverage = %+v, want total 2 / withEVIC 1", cov)
	}
	if len(cov.WithoutEVIC) != 1 || cov.WithoutEVIC[0] != "XOM" {
		t.Errorf("no-EVIC list = %v, want [XOM]", cov.WithoutEVIC)
	}
}

func TestIngest_HoldingsJoinAndPCAFSkip(t *testing.T) {
	idx, _ := Ingest(context.Background(), fakeESGSource{rows: sampleRows()})
	positions := map[string]float64{"AAPL": 1_000_000, "XOM": 500_000, "TSLA": 250_000}
	holdings, uncovered := idx.Holdings(positions)
	if len(holdings) != 3 {
		t.Fatalf("got %d holdings, want 3", len(holdings))
	}
	// TSLA has no ESG coverage → reported uncovered + zero issuer datum.
	if len(uncovered) != 1 || uncovered[0] != "TSLA" {
		t.Errorf("uncovered = %v, want [TSLA]", uncovered)
	}

	// PCAF financed emissions: AAPL contributes (1e6/3e6)·5300 ≈ 1766.7; XOM
	// (no EVIC) and TSLA (no data) are skipped — the coverage gap, not zeroed-in.
	fe, _ := FinancedEmissions(holdings)
	want := (1_000_000.0 / 3_000_000.0) * (100 + 200 + 5000)
	if fe < want-0.5 || fe > want+0.5 {
		t.Errorf("financed emissions = %.2f, want ~%.2f (only AAPL counts)", fe, want)
	}
}

func TestIngest_Error(t *testing.T) {
	if _, err := Ingest(context.Background(), fakeESGSource{err: errors.New("vendor down")}); err == nil {
		t.Error("expected the source error to propagate")
	}
}
