package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/xva"
)

type staticXVA struct{ exp []xva.Adjustments }

func (s staticXVA) Exposures(context.Context, time.Time) ([]xva.Adjustments, bool) {
	return s.exp, s.exp != nil
}

func xvaTestAdjustments() []xva.Adjustments {
	prof := xva.ExposureProfile{
		Times: []float64{1, 2, 3},
		EE:    []float64{500_000, 400_000, 300_000},
		PFE:   []float64{900_000, 1_200_000, 800_000},
	}
	return []xva.Adjustments{{Profile: prof, Counterparty: xva.FromCDS(0.01, 0.4), DiscountRate: 0.02}}
}

func TestRegisterXVA_CVAandPFE(t *testing.T) {
	r := DefaultRegistry()
	RegisterXVA(context.Background(), r, staticXVA{exp: xvaTestAdjustments()})
	p := domain.NewPortfolio("p1", "USD")
	ms := ComputeMeasures(p, r, nil)

	cva, ok := ms.Lookup(MeasureCVA)
	if !ok || decutil.Float64Or(cva.Value, 0) <= 0 {
		t.Fatalf("CVA must be positive, got %.2f ok=%v", decutil.Float64Or(cva.Value, 0), ok)
	}
	pfe, _ := ms.Lookup(MeasurePFE)
	// Peak PFE across the profile = 1,200,000.
	if decutil.Float64Or(pfe.Value, 0) != 1_200_000 {
		t.Fatalf("PFE must be the peak 1,200,000, got %.2f", decutil.Float64Or(pfe.Value, 0))
	}
}

func TestRegisterXVA_NoProviderIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterXVA(context.Background(), r, staticXVA{exp: nil})
	ms := ComputeMeasures(domain.NewPortfolio("p1", "USD"), r, nil)
	cva, _ := ms.Lookup(MeasureCVA)
	if decutil.Float64Or(cva.Value, 0) != 0 {
		t.Fatalf("CVA with no provider must be 0, got %.2f", decutil.Float64Or(cva.Value, 0))
	}
}
