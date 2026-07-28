package compute

import (
	"context"
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
	if !ok || decimalToFloat(cva.Value) <= 0 {
		t.Fatalf("CVA must be positive, got %.2f ok=%v", decimalToFloat(cva.Value), ok)
	}
	pfe, _ := ms.Lookup(MeasurePFE)
	// Peak PFE across the profile = 1,200,000.
	if decimalToFloat(pfe.Value) != 1_200_000 {
		t.Fatalf("PFE must be the peak 1,200,000, got %.2f", decimalToFloat(pfe.Value))
	}
}

func TestRegisterXVA_NoProviderIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterXVA(context.Background(), r, staticXVA{exp: nil})
	ms := ComputeMeasures(domain.NewPortfolio("p1", "USD"), r, nil)
	cva, _ := ms.Lookup(MeasureCVA)
	if decimalToFloat(cva.Value) != 0 {
		t.Fatalf("CVA with no provider must be 0, got %.2f", decimalToFloat(cva.Value))
	}
}
