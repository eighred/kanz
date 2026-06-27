package library

import (
	"sort"

	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
)

// Curve-shift scenario catalog (FI-01e) — the rate-world counterpart of the
// GICS-curve equity scenarios in library.go. Each entry is a curve.Shift the
// scenario engine reprices bond positions against (scenario.EvaluateCurveShift),
// landing the MODEL-01 curve-shift carried-forward.
//
// The three canonical shapes — parallel, steepener, butterfly — are the standard
// rate-risk stress axes a fixed-income desk runs: a parallel move tests
// duration, a steepener/flattener tests slope (key-rate) exposure, a butterfly
// tests curvature. Magnitudes are illustrative defaults a quant recalibrates;
// the builders take basis points so a desk constructs a custom shift directly.

// CurveParallel is a parallel shift of bp basis points (positive = rates up).
func CurveParallel(bp float64) curve.Shift { return curve.Parallel{Bp: bp} }

// CurveSteepener ramps the curve from −bp/2 at the short end to +bp/2 at the
// long end — a pivot steepener (positive bp steepens, negative flattens).
func CurveSteepener(bp float64) curve.Shift {
	return curve.Steepener{ShortBp: -bp / 2, LongBp: bp / 2}
}

// CurveButterfly bumps the belly by bp relative to the wings (positive = belly
// cheapens).
func CurveButterfly(bp float64) curve.Shift { return curve.Butterfly{BellyBp: bp} }

// NamedCurveShift looks up a curve scenario by catalog name, returning the shift
// and true, or nil and false when unknown — the dispatch seam an API/CLI uses to
// drive scenario.EvaluateCurveShift from a name.
func NamedCurveShift(name string) (curve.Shift, bool) {
	b, ok := curveCatalog[name]
	if !ok {
		return nil, false
	}
	return b(), true
}

// CurveShiftNames returns the curve catalog's scenario names in stable order.
func CurveShiftNames() []string {
	out := make([]string, 0, len(curveCatalog))
	for name := range curveCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// curveCatalog is the name→builder registry NamedCurveShift reads. The defaults
// span the three shapes at conventional magnitudes (100bp parallel, 50bp slope,
// 25bp curvature).
var curveCatalog = map[string]func() curve.Shift{
	"RATES_UP_100":   func() curve.Shift { return CurveParallel(100) },
	"RATES_DOWN_100": func() curve.Shift { return CurveParallel(-100) },
	"BEAR_STEEPENER": func() curve.Shift { return CurveSteepener(50) },
	"BULL_FLATTENER": func() curve.Shift { return CurveSteepener(-50) },
	"BUTTERFLY_25":   func() curve.Shift { return CurveButterfly(25) },
}
