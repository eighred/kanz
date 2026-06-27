package library

import (
	"sort"

	"github.com/kanz-eng/kanz/internal/risk/liquidity"
)

// Liquidity-stress scenario catalog (LIQ-01d) — the liquidity-axis counterpart of
// the GICS price curves (library.go) and the rate curves (curve.go). A liquidity
// crisis widens spreads and dries up volume rather than moving prices, so each
// entry is a liquidity.Stress (a transform on the liquidity Provider) the
// liquidity measures recompute under, not a price ScenarioShock. Under stress the
// liquidation horizon lengthens (ADV down) and the liquidation cost / LVaR rises
// (spreads up) — the exact dimension a price-only scenario cannot express.
//
// Magnitudes are illustrative defaults a quant recalibrates; a desk constructs a
// custom stress directly via liquidity.Stress.

// LiquidityStress is the canonical funding-stress regime: spreads triple and
// ADV falls to a third (the horizon roughly triples with it).
func LiquidityStress() liquidity.Stress {
	return liquidity.Stress{SpreadMult: 3, ADVMult: 1.0 / 3.0}
}

// LiquidityCrisis is the severe tail: spreads ×5 and ADV to a fifth (a 2008-style
// evaporation of liquidity).
func LiquidityCrisis() liquidity.Stress {
	return liquidity.Stress{SpreadMult: 5, ADVMult: 0.2}
}

// NamedLiquidityStress looks up a liquidity-stress scenario by catalog name,
// returning the stress and true, or the zero Stress and false when unknown — the
// dispatch seam an API/CLI uses to drive the liquidity measures under stress.
func NamedLiquidityStress(name string) (liquidity.Stress, bool) {
	b, ok := liquidityCatalog[name]
	if !ok {
		return liquidity.Stress{}, false
	}
	return b(), true
}

// LiquidityStressNames returns the liquidity-stress catalog names in stable order.
func LiquidityStressNames() []string {
	out := make([]string, 0, len(liquidityCatalog))
	for name := range liquidityCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// liquidityCatalog is the name→builder registry NamedLiquidityStress reads.
var liquidityCatalog = map[string]func() liquidity.Stress{
	"LIQUIDITY_STRESS": LiquidityStress,
	"LIQUIDITY_CRISIS": LiquidityCrisis,
}
