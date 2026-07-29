package compute_test

import (
	"math"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

func hhiVal(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

func TestHHI(t *testing.T) {
	tests := []struct {
		name string
		pos  []domain.Position
		want float64
	}{
		{"single position is fully concentrated",
			[]domain.Position{{InstrumentID: "AAPL", MarketValue: mkMoney(1000, 0, "USD")}}, 1.0},
		{"two equal positions ⇒ 1/n",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(500, 0, "USD")},
				{InstrumentID: "MSFT", MarketValue: mkMoney(500, 0, "USD")},
			}, 0.5},
		{"four equal positions ⇒ 0.25",
			[]domain.Position{
				{InstrumentID: "A", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "B", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "C", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "D", MarketValue: mkMoney(250, 0, "USD")},
			}, 0.25},
		{"mixed 750/250 ⇒ 0.625",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(750, 0, "USD")},
				{InstrumentID: "MSFT", MarketValue: mkMoney(250, 0, "USD")},
			}, 0.625},
		{"non-base-currency positions are skipped",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(1000, 0, "USD")},
				{InstrumentID: "VOD.L", MarketValue: mkMoney(9999, 0, "EUR")},
			}, 1.0},
		{"empty portfolio ⇒ 0", nil, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := compute.HHI(makePortfolio("P1", "USD", tc.pos...))
			if m.Name != compute.MeasureHHI {
				t.Fatalf("name = %q, want HHI", m.Name)
			}
			if got := hhiVal(m.Value); math.Abs(got-tc.want) > 1e-4 {
				t.Fatalf("HHI = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHHIRegisteredInDefault(t *testing.T) {
	r := compute.DefaultRegistry()
	found := false
	for _, n := range r.Names() {
		if n == compute.MeasureHHI {
			found = true
		}
	}
	if !found {
		t.Fatal("HHI must be in DefaultRegistry (positions-only, always available)")
	}
}
