package compute

import (
	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
	"github.com/kanz-eng/kanz/internal/risk/pricing/volsurface"
)

// The PARITY-03a/03b point-in-time stores satisfy the FI-01d / DERIV-01d
// provider seams — the assertions live here because the pricing packages
// cannot import compute.
var (
	_ CurveProvider = (*curve.Store)(nil)
	_ VolProvider   = (*volsurface.Store)(nil)
)
