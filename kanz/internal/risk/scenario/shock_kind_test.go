package scenario

import (
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

type unsupportedShock struct{}

func (unsupportedShock) Description() string { return "unsupported" }

func TestKnownShockKindRecognizesOnlyTheV1Vocabulary(t *testing.T) {
	for _, shock := range []v1.ScenarioShock{
		v1.PriceShock{},
		v1.ParallelShift{},
		v1.SectorShock{},
		v1.VolShock{},
	} {
		if !knownShockKind(shock) {
			t.Errorf("knownShockKind(%T) = false", shock)
		}
	}
	if knownShockKind(unsupportedShock{}) {
		t.Error("unsupported shock was accepted")
	}
}

func TestKnownShockKindAllocatesNothing(t *testing.T) {
	shock := v1.ScenarioShock(v1.PriceShock{})
	if got := testing.AllocsPerRun(1000, func() {
		if !knownShockKind(shock) {
			t.Fatal("known shock rejected")
		}
	}); got != 0 {
		t.Fatalf("knownShockKind allocations/run = %v, want 0", got)
	}
}
