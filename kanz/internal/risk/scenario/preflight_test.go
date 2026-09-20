package scenario

import (
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"testing"
)

func TestShockPreflightZeroAllocations(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		shocks := []v1.ScenarioShock{v1.PriceShock{}, v1.ParallelShift{}, v1.SectorShock{}, v1.VolShock{}}
		if unknown {
			shocks = append(shocks, nil)
		}
		var got bool
		allocs := testing.AllocsPerRun(1000, func() { got = hasUnknownShock(shocks) })
		if got != unknown || allocs != 0 {
			t.Fatalf("unknown=%v: result=%v allocations=%v", unknown, got, allocs)
		}
	}
}
