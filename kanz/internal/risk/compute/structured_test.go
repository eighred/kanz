package compute

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
	"github.com/kanz-eng/kanz/internal/risk/pricing/structured"
)

type staticStructured map[string]StructuredSpec

func (m staticStructured) Structured(_ context.Context, id string, _ time.Time) (StructuredSpec, bool) {
	s, ok := m[id]
	return s, ok
}

func structTestSpec(t *testing.T) StructuredSpec {
	t.Helper()
	c, err := curve.NewZeroCurve([]float64{1, 5, 10, 30}, []float64{0.03, 0.035, 0.04, 0.045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	return StructuredSpec{
		Deal: structured.Deal{
			Pool:     structured.Pool{Balance: 1_000_000, GrossCoupon: 0.06, ServicingFee: 0.005, TermMonths: 360},
			Tranches: []structured.Tranche{{Name: "A", Balance: 800_000, Coupon: 0.04}, {Name: "B", Balance: 200_000, Coupon: 0.06}},
		},
		Prepay:       structured.Behavioral{Base: 0.05, Max: 0.45, Steepness: 40, CDR: 0.01, Sev: 0.35},
		Env:          structured.RateEnv{Curve: c, RefTenor: 10},
		TrancheIndex: 0,
		OAS:          0.01,
	}
}

func TestRegisterStructuredRisk(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, staticStructured{"MBS_A": structTestSpec(t)})

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MBS_A", MarketValue: &commonpb.Money{Amount: dec(950_000, 0), CurrencyCode: "USD"}})
	p.SetPosition(domain.Position{InstrumentID: "EQ", MarketValue: &commonpb.Money{Amount: dec(50_000, 0), CurrencyCode: "USD"}}) // non-structured

	ms := ComputeMeasures(p, r, nil)
	dur, ok := ms.Lookup(MeasureStructDuration)
	if !ok || decimalToFloat(dur.Value) <= 0 {
		t.Fatalf("StructDuration must be positive, got %.4f ok=%v", decimalToFloat(dur.Value), ok)
	}
	wal, _ := ms.Lookup(MeasureStructWAL)
	if decimalToFloat(wal.Value) <= 0 {
		t.Fatalf("StructWAL must be positive, got %.4f", decimalToFloat(wal.Value))
	}
}

func TestRegisterStructuredRisk_NonStructuredIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, staticStructured{})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", MarketValue: &commonpb.Money{Amount: dec(50_000, 0), CurrencyCode: "USD"}})
	ms := ComputeMeasures(p, r, nil)
	dur, _ := ms.Lookup(MeasureStructDuration)
	if decimalToFloat(dur.Value) != 0 {
		t.Fatalf("StructDuration with no structured positions must be 0, got %.4f", decimalToFloat(dur.Value))
	}
}
