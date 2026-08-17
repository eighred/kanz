package compute

import (
	"context"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/xva"
)

// XVA-01e wires counterparty credit risk into the RISK-07 measure registry — the
// XVA sibling of the FI/Greek/factor/liquidity enrichment. It registers the
// portfolio-level CVA (the price of counterparty default risk, summed across
// counterparties) and the peak PFE (the headline counterparty exposure number),
// each resolving the portfolio's per-counterparty exposure profiles + credit
// curves at p.AsOf() through an XVAProvider.
//
// The exposure Monte-Carlo (XVA-01b) is expensive, so — unlike the cheap FI/Greek
// per-position reprices — the provider supplies PRECOMPUTED per-counterparty
// xva.Adjustments (the profile + curves), and the measure does only the cheap
// CVA quadrature / peak-PFE reduction. The simulation runs upstream (a nightly
// exposure job), the measure reads its output.
//
//	CVA = Σ_counterparty Adjustments.CVA()
//	PFE = max_counterparty Profile.PeakPFE()

// XVA measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureCVA v1.MeasureName = "CVA"
	MeasurePFE v1.MeasureName = "PFE"
)

const xvaExp int32 = -2 // cents — CVA/PFE are money amounts in the base currency

// XVAProvider resolves the portfolio's per-counterparty exposure adjustments as
// of a knowledge horizon — the per-snapshot enrichment seam. ok=false ⇒ no XVA
// context (the measures report zero rather than failing the recompute, and mark
// that zero unmeasured — see SkipNoExposures).
type XVAProvider interface {
	Exposures(ctx context.Context, asOf time.Time) ([]xva.Adjustments, bool)
}

// SkipNoExposures: no counterparty exposure context resolved for this snapshot —
// the provider declined, or none is wired. The whole-evaluation reason, the XVA
// sibling of SkipNoModel.
//
// A CVA OF ZERO IS THE MOST FLATTERING NUMBER THIS ENGINE CAN EMIT: it says the
// book carries no counterparty default risk at all. #509's audit found this
// branch returning exactly that, unmarked, and it is harmless only because the
// XVA measures are still unregistered — which is a schedule, not a safeguard.
const SkipNoExposures = "no_exposures"

// RegisterXVA registers CVA and PFE on r, each closing over ctx + the provider.
// Call at engine startup after DefaultRegistry; tests register against a static
// provider.
func RegisterXVA(ctx context.Context, r *Registry, provider XVAProvider) {
	r.Register(MeasureCVA, xvaMeasure(ctx, provider, MeasureCVA))
	r.Register(MeasurePFE, xvaMeasure(ctx, provider, MeasurePFE))
}

func xvaMeasure(ctx context.Context, provider XVAProvider, name v1.MeasureName) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		var cov Coverage
		// A NIL PROVIDER USED TO PANIC HERE. fi.go, greeks.go and factorrisk.go all
		// absorb one; this seam alone dereferenced it, so the misconfiguration
		// registered fine and took the recompute goroutine down on the first
		// query. Absorbed to match its siblings — and absorbed as an exclusion, so
		// it is a marked zero rather than a quiet one.
		exposures, ok := []xva.Adjustments(nil), false
		if provider != nil {
			exposures, ok = provider.Exposures(ctx, p.AsOf())
		}
		if !ok {
			cov.ExcludeWhole(SkipNoExposures)
			return v1.Measure{Name: name, Value: floatToDecimal(0, xvaExp), Coverage: cov.Result()}
		}
		var val float64
		for _, a := range exposures {
			cov.Contributed++
			switch name {
			case MeasureCVA:
				val += a.CVA()
			default: // MeasurePFE — the peak across counterparties
				if pk := a.Profile.PeakPFE(); pk > val {
					val = pk
				}
			}
		}
		return v1.Measure{Name: name, Value: floatToDecimal(val, xvaExp), Coverage: cov.Result()}
	}
}
