package integrity

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

// DriftMetric names the statistical measure a detector uses.
type DriftMetric string

// MetricPSI is the Population Stability Index — the canonical input-
// drift metric. Other metrics (KL divergence, KS statistic) can plug in
// as separate detectors carrying their own DriftMetric label; the
// DriftResult shape (score vs threshold over a window) is metric-
// agnostic so observation.v1.DriftDetail (DATA-07) consumes any of them.
const MetricPSI DriftMetric = "psi"

// DefaultPSIThreshold is the conventional "significant shift" cutoff for
// PSI: < 0.1 negligible, 0.1–0.25 moderate, > 0.25 significant. We alarm
// above 0.25.
const DefaultPSIThreshold = 0.25

// psiEpsilon floors a bin proportion so an empty bin doesn't blow up the
// ln() / division in the PSI sum.
const psiEpsilon = 1e-6

// DriftResult is the outcome of assessing one observation window against
// the baseline. Maps onto observation.v1.DriftDetail (DATA-07).
type DriftResult struct {
	Feature   string
	Metric    DriftMetric
	Score     float64
	Threshold float64

	// Drifted is Score > Threshold AND the window had enough samples
	// (Sufficient) — the condition DATA-07 turns into a DriftDetail event.
	Drifted bool

	// Sufficient reports whether the window met MinSamples. PSI on a
	// handful of samples is noise; an insufficient window is never
	// Drifted regardless of score.
	Sufficient  bool
	SampleCount uint64

	WindowStart time.Time
	WindowEnd   time.Time
}

// PSIDetector tracks the binned distribution of one feature over the
// current window and scores it against a fixed baseline distribution via
// PSI. One detector per feature.
//
// Detection only — DATA-07 emits the DriftDetail event; the caller owns
// window boundaries (call Assess at window close, then Reset). In-memory
// and per-process, concurrent-safe.
type PSIDetector struct {
	feature    string
	edges      []float64 // sorted internal bin boundaries; len(edges)+1 bins
	baseline   []float64 // baseline proportion per bin, normalized to sum 1
	threshold  float64
	minSamples uint64

	mu     sync.Mutex
	counts []uint64
	total  uint64
}

// PSIConfig configures a detector. Edges are the internal bin boundaries
// (sorted, strictly ascending); they define len(Edges)+1 bins. Baseline
// is the reference mass per bin (counts or proportions — normalized
// internally); its length must be len(Edges)+1.
type PSIConfig struct {
	Feature    string
	Edges      []float64
	Baseline   []float64
	Threshold  float64 // ≤ 0 ⇒ DefaultPSIThreshold
	MinSamples uint64  // window must have at least this many samples to alarm; 0 ⇒ always sufficient
}

// NewPSIDetector validates the config and returns a detector.
func NewPSIDetector(cfg PSIConfig) (*PSIDetector, error) {
	if cfg.Feature == "" {
		return nil, errors.New("integrity: PSIConfig.Feature required")
	}
	if len(cfg.Baseline) != len(cfg.Edges)+1 {
		return nil, errors.New("integrity: len(Baseline) must equal len(Edges)+1")
	}
	if len(cfg.Baseline) < 2 {
		return nil, errors.New("integrity: need at least 2 bins")
	}
	for i := 1; i < len(cfg.Edges); i++ {
		if cfg.Edges[i] <= cfg.Edges[i-1] {
			return nil, errors.New("integrity: Edges must be strictly ascending")
		}
	}
	var sum float64
	for _, b := range cfg.Baseline {
		if b < 0 {
			return nil, errors.New("integrity: Baseline values must be non-negative")
		}
		sum += b
	}
	if sum <= 0 {
		return nil, errors.New("integrity: Baseline must have positive total mass")
	}

	baseline := make([]float64, len(cfg.Baseline))
	for i, b := range cfg.Baseline {
		baseline[i] = b / sum // normalize to proportions
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = DefaultPSIThreshold
	}
	edges := append([]float64(nil), cfg.Edges...) // defensive copy

	return &PSIDetector{
		feature:    cfg.Feature,
		edges:      edges,
		baseline:   baseline,
		threshold:  threshold,
		minSamples: cfg.MinSamples,
		counts:     make([]uint64, len(baseline)),
	}, nil
}

// Observe records one feature value into the current window's histogram.
func (d *PSIDetector) Observe(value float64) {
	bin := sort.SearchFloat64s(d.edges, value) // 0..len(edges)
	d.mu.Lock()
	d.counts[bin]++
	d.total++
	d.mu.Unlock()
}

// Assess scores the current window against the baseline and returns the
// result. Non-destructive — call Reset to start a new window. The window
// bounds are caller-supplied (the detector tracks samples, not time) and
// flow through to DriftResult for the DriftDetail event.
func (d *PSIDetector) Assess(windowStart, windowEnd time.Time) DriftResult {
	d.mu.Lock()
	total := d.total
	counts := append([]uint64(nil), d.counts...)
	d.mu.Unlock()

	res := DriftResult{
		Feature:     d.feature,
		Metric:      MetricPSI,
		Threshold:   d.threshold,
		SampleCount: total,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Sufficient:  total > 0 && total >= d.minSamples,
	}
	if total == 0 {
		return res // no samples ⇒ score 0, not drifted
	}

	var psi float64
	for i, c := range counts {
		actual := float64(c) / float64(total)
		expected := d.baseline[i]
		if actual < psiEpsilon {
			actual = psiEpsilon
		}
		if expected < psiEpsilon {
			expected = psiEpsilon
		}
		psi += (actual - expected) * math.Log(actual/expected)
	}
	res.Score = psi
	res.Drifted = res.Sufficient && psi > d.threshold
	return res
}

// Reset clears the current window's histogram. Call at window rollover
// after Assess. The baseline is unchanged.
func (d *PSIDetector) Reset() {
	d.mu.Lock()
	for i := range d.counts {
		d.counts[i] = 0
	}
	d.total = 0
	d.mu.Unlock()
}
