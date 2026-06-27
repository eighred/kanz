package library

import "sort"

// Factor-shock scenario catalog (FACTOR-01e) — the factor-space counterpart of
// the GICS price curves, the rate curves, and the liquidity stresses. Each entry
// is a map of factor name → factor-return shock the scenario engine reprices a
// book against (scenario.EvaluateFactorShock), so a desk runs "what if momentum
// crashes and value rallies?" — a regime no single price or sector shift can
// express. The factor names match the fundamental model's style factors
// (FACTOR-01b); magnitudes are illustrative single-period moves a quant
// recalibrates.

// FactorShock is a named set of factor-return shocks (factor name → return).
type FactorShock = map[string]float64

// MomentumCrash is the classic factor reversal: momentum sells off hard while
// value rallies (the unwind that whipsawed quant books in 2009 and 2020).
func MomentumCrash() FactorShock {
	return FactorShock{"Momentum": -0.10, "Value": 0.05}
}

// FlightToQuality is a risk-off rotation: low-volatility and quality bid, small-
// cap (size) and high-beta sold.
func FlightToQuality() FactorShock {
	return FactorShock{"Quality": 0.04, "Volatility": -0.06, "Size": -0.05}
}

// NamedFactorShock looks up a factor scenario by catalog name, returning the
// shock and true, or nil and false when unknown — the dispatch seam an API/CLI
// uses to drive scenario.EvaluateFactorShock from a name.
func NamedFactorShock(name string) (FactorShock, bool) {
	b, ok := factorCatalog[name]
	if !ok {
		return nil, false
	}
	return b(), true
}

// FactorShockNames returns the factor-shock catalog names in stable order.
func FactorShockNames() []string {
	out := make([]string, 0, len(factorCatalog))
	for name := range factorCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// factorCatalog is the name→builder registry NamedFactorShock reads.
var factorCatalog = map[string]func() FactorShock{
	"MOMENTUM_CRASH":    MomentumCrash,
	"FLIGHT_TO_QUALITY": FlightToQuality,
}
