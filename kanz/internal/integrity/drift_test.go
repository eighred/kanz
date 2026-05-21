package integrity_test

import (
	"math"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/integrity"
)

var driftWin = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// twoBinDetector: edges=[0.0] → bin0 = values ≤ 0, bin1 = values > 0;
// baseline 50/50.
func twoBinDetector(t *testing.T, threshold float64, minSamples uint64) *integrity.PSIDetector {
	t.Helper()
	d, err := integrity.NewPSIDetector(integrity.PSIConfig{
		Feature:    "return_5d",
		Edges:      []float64{0.0},
		Baseline:   []float64{0.5, 0.5},
		Threshold:  threshold,
		MinSamples: minSamples,
	})
	if err != nil {
		t.Fatalf("NewPSIDetector: %v", err)
	}
	return d
}

func observeN(d *integrity.PSIDetector, value float64, n int) {
	for i := 0; i < n; i++ {
		d.Observe(value)
	}
}

// --- Validation -------------------------------------------------------

func TestNewPSIDetector_Validation(t *testing.T) {
	cases := map[string]integrity.PSIConfig{
		"empty feature":     {Feature: "", Edges: []float64{0}, Baseline: []float64{0.5, 0.5}},
		"baseline len":      {Feature: "f", Edges: []float64{0}, Baseline: []float64{1}},
		"too few bins":      {Feature: "f", Edges: []float64{}, Baseline: []float64{1}},
		"edges not ascend":  {Feature: "f", Edges: []float64{1, 1}, Baseline: []float64{1, 1, 1}},
		"negative baseline": {Feature: "f", Edges: []float64{0}, Baseline: []float64{-1, 2}},
		"zero mass":         {Feature: "f", Edges: []float64{0}, Baseline: []float64{0, 0}},
	}
	for name, cfg := range cases {
		if _, err := integrity.NewPSIDetector(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewPSIDetector_DefaultThreshold(t *testing.T) {
	d := twoBinDetector(t, 0, 0) // threshold 0 ⇒ default
	observeN(d, -1, 5)
	observeN(d, 1, 5)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if r.Threshold != integrity.DefaultPSIThreshold {
		t.Errorf("Threshold=%v want default %v", r.Threshold, integrity.DefaultPSIThreshold)
	}
}

// --- PSI score correctness --------------------------------------------

func TestAssess_IdenticalToBaselineIsZeroPSI(t *testing.T) {
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 50) // bin0
	observeN(d, 1, 50)  // bin1 → actual 0.5/0.5 == baseline
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if math.Abs(r.Score) > 1e-9 {
		t.Errorf("Score=%v want ~0 for identical distribution", r.Score)
	}
	if r.Drifted {
		t.Error("Drifted true for identical distribution")
	}
}

func TestAssess_KnownPSIValue(t *testing.T) {
	// actual 60/40 vs baseline 50/50:
	// PSI = (.6-.5)ln(.6/.5) + (.4-.5)ln(.4/.5) ≈ 0.040546
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 6)
	observeN(d, 1, 4)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	want := 0.040546
	if math.Abs(r.Score-want) > 1e-4 {
		t.Errorf("Score=%v want ≈%v", r.Score, want)
	}
	if r.Drifted {
		t.Error("0.04 PSI should not drift at threshold 0.25")
	}
	if r.SampleCount != 10 {
		t.Errorf("SampleCount=%d want 10", r.SampleCount)
	}
}

func TestAssess_SignificantShiftDrifts(t *testing.T) {
	// actual 90/10 vs 50/50 → PSI ≈ 0.879 > 0.25.
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 9)
	observeN(d, 1, 1)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if !r.Drifted {
		t.Errorf("Drifted false; Score=%v should exceed 0.25", r.Score)
	}
}

func TestAssess_EmptyBinDoesNotProduceNaN(t *testing.T) {
	// All mass in bin0; bin1 empty → epsilon floor keeps PSI finite.
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 20)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if math.IsNaN(r.Score) || math.IsInf(r.Score, 0) {
		t.Fatalf("Score=%v want finite (epsilon floor)", r.Score)
	}
	if !r.Drifted {
		t.Error("a fully-collapsed distribution should drift")
	}
}

// --- Sufficiency + windows --------------------------------------------

func TestAssess_InsufficientSamplesNeverDrifts(t *testing.T) {
	d := twoBinDetector(t, 0.25, 100) // require 100 samples
	observeN(d, -1, 9)                // heavy skew but only 10 samples
	observeN(d, 1, 1)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if r.Sufficient {
		t.Error("Sufficient true below MinSamples")
	}
	if r.Drifted {
		t.Error("Drifted true despite insufficient samples")
	}
	if r.Score <= 0.25 {
		t.Errorf("Score=%v expected high (skewed); sufficiency, not score, gates Drifted", r.Score)
	}
}

func TestAssess_ZeroSamples(t *testing.T) {
	d := twoBinDetector(t, 0.25, 0)
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if r.SampleCount != 0 || r.Drifted || r.Score != 0 {
		t.Errorf("empty window: count=%d drifted=%v score=%v want 0,false,0", r.SampleCount, r.Drifted, r.Score)
	}
}

func TestReset_ClearsWindow(t *testing.T) {
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 9)
	observeN(d, 1, 1)
	if r := d.Assess(driftWin, driftWin.Add(time.Minute)); !r.Drifted {
		t.Fatal("setup: expected drift before reset")
	}
	d.Reset()
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	if r.SampleCount != 0 {
		t.Errorf("SampleCount=%d after reset want 0", r.SampleCount)
	}
}

func TestAssess_CarriesWindowAndMetadata(t *testing.T) {
	d := twoBinDetector(t, 0.25, 0)
	observeN(d, -1, 5)
	observeN(d, 1, 5)
	start := driftWin
	end := driftWin.Add(15 * time.Minute)
	r := d.Assess(start, end)
	if r.Feature != "return_5d" || r.Metric != integrity.MetricPSI {
		t.Errorf("Feature=%q Metric=%q want return_5d/psi", r.Feature, r.Metric)
	}
	if !r.WindowStart.Equal(start) || !r.WindowEnd.Equal(end) {
		t.Errorf("window = [%v,%v] want [%v,%v]", r.WindowStart, r.WindowEnd, start, end)
	}
}

// --- Binning ----------------------------------------------------------

func TestObserve_MultiBinPlacement(t *testing.T) {
	// edges=[0,10] → bin0 (≤0), bin1 (0<v≤10), bin2 (>10); baseline thirds.
	d, err := integrity.NewPSIDetector(integrity.PSIConfig{
		Feature:  "px",
		Edges:    []float64{0, 10},
		Baseline: []float64{1, 1, 1},
	})
	if err != nil {
		t.Fatalf("NewPSIDetector: %v", err)
	}
	observeN(d, -5, 10) // bin0
	observeN(d, 5, 10)  // bin1
	observeN(d, 50, 10) // bin2
	r := d.Assess(driftWin, driftWin.Add(time.Minute))
	// even thirds vs even-thirds baseline → ~0 PSI.
	if math.Abs(r.Score) > 1e-9 {
		t.Errorf("Score=%v want ~0 (matches baseline thirds)", r.Score)
	}
	if r.SampleCount != 30 {
		t.Errorf("SampleCount=%d want 30", r.SampleCount)
	}
}
