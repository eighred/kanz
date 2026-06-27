package library

import (
	"sort"

	"github.com/kanz-eng/kanz/internal/risk/xva"
)

// Credit-stress scenario catalog (XVA-01e) — the credit-axis counterpart of the
// GICS price curves, the rate curves, the liquidity stresses, and the factor
// shocks. Each entry is an xva.CreditStress (a transform on the counterparty
// credit curve) the XVA layer recomputes CVA under: a spread-widening regime
// raises hazard rates (and CVA with them), a counterparty default crystallizes
// the near-term loss. Magnitudes are illustrative defaults a quant recalibrates.

// CreditSpreadWidening triples hazard rates — a broad credit-spread blowout.
func CreditSpreadWidening() xva.CreditStress {
	return xva.CreditStress{HazardMult: 3}
}

// CounterpartyDefault realizes an immediate jump-to-default.
func CounterpartyDefault() xva.CreditStress {
	return xva.CreditStress{JumpToDefault: true}
}

// NamedCreditStress looks up a credit scenario by catalog name, returning the
// stress and true, or the zero CreditStress and false when unknown — the dispatch
// seam an API/CLI uses to drive the XVA layer under stress.
func NamedCreditStress(name string) (xva.CreditStress, bool) {
	b, ok := creditCatalog[name]
	if !ok {
		return xva.CreditStress{}, false
	}
	return b(), true
}

// CreditStressNames returns the credit catalog names in stable order.
func CreditStressNames() []string {
	out := make([]string, 0, len(creditCatalog))
	for name := range creditCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var creditCatalog = map[string]func() xva.CreditStress{
	"CREDIT_SPREAD_WIDENING": CreditSpreadWidening,
	"COUNTERPARTY_DEFAULT":   CounterpartyDefault,
}
