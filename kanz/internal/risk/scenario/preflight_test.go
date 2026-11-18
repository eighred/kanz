package scenario

import (
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestShockPreflightZeroAllocations(t *testing.T) {
	zero := &commonpb.Decimal{}
	valid := []v1.ScenarioShock{v1.PriceShock{InstrumentID: "A", Pct: zero}, v1.ParallelShift{Pct: zero}, v1.SectorShock{Taxonomy: "GICS", Code: "10", Pct: zero}, v1.VolShock{AbsBump: zero}}
	for _, tc := range []struct {
		shocks []v1.ScenarioShock
		want   shockPreflight
	}{
		{valid, shockPreflight{}},
		{append(valid, nil), shockPreflight{unknownType: true}},
		{[]v1.ScenarioShock{v1.PriceShock{InstrumentID: "A"}}, shockPreflight{invalidMagnitude: true}},
		{[]v1.ScenarioShock{v1.PriceShock{InstrumentID: " ", Pct: zero}}, shockPreflight{missingInstruments: 1}},
	} {
		var got shockPreflight
		allocs := testing.AllocsPerRun(1000, func() { got = hasUnknownShock(tc.shocks) })
		if got != tc.want || allocs != 0 {
			t.Fatalf("got preflight=%+v allocations=%v, want %+v with zero allocations", got, allocs, tc.want)
		}
	}
}
