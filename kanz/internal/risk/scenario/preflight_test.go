package scenario

import (
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"testing"
)

func TestShockPreflightZeroAllocations(t *testing.T) {
	zero := &commonpb.Decimal{}
	valid := []v1.ScenarioShock{v1.PriceShock{Pct: zero}, v1.ParallelShift{Pct: zero}, v1.SectorShock{Pct: zero}, v1.VolShock{AbsBump: zero}}
	for _, tc := range []struct {
		shocks           []v1.ScenarioShock
		unknown, invalid bool
	}{
		{valid, false, false},
		{append(valid, nil), true, false},
		{[]v1.ScenarioShock{v1.PriceShock{}}, false, true},
	} {
		var unknown, invalid bool
		allocs := testing.AllocsPerRun(1000, func() { unknown, invalid = hasUnknownShock(tc.shocks) })
		if unknown != tc.unknown || invalid != tc.invalid || allocs != 0 {
			t.Fatalf("unknown=%v invalid=%v allocations=%v", unknown, invalid, allocs)
		}
	}
}
