package compute

import (
	"context"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// STRUCT-01d wires structured-product rate risk into the RISK-07 measure registry
// — the securitized sibling of the FI-01d bond measures. It registers effective
// duration, effective convexity, and weighted-average life, each repricing a
// structured position through the STRUCT-01b waterfall + STRUCT-01c prepay model
// resolved point-in-time, the same enrichment seam as FI/Greeks.
//
//	StructDuration  = Σ |MV_i|·effDur_i / Σ |MV_i|     (MV-weighted years)
//	StructConvexity = Σ |MV_i|·effConv_i / Σ |MV_i|    (negative for callable MBS)
//	StructWAL       = Σ |MV_i|·WAL_i / Σ |MV_i|        (MV-weighted years)
//
// A non-structured position contributes nothing.

// Structured measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureStructDuration  v1.MeasureName = "StructDuration"
	MeasureStructConvexity v1.MeasureName = "StructConvexity"
	MeasureStructWAL       v1.MeasureName = "StructWAL"
)

// structRiskExp is the precision the dimensionless structured measures emit at.
const structRiskExp int32 = -4

// structDurationBumpBp is the parallel shift the effective duration/convexity
// reprice under.
const structDurationBumpBp = 25.0

// StructuredSpec is the per-instrument structured-product pricing context — the
// deal, the prepay model, the rate environment, the held tranche, and the OAS the
// position is marked at. The float working shape behind reference.v1.
// StructuredTerms, resolved by a StructuredProvider.
type StructuredSpec struct {
	Deal         structured.Deal
	Prepay       structured.PrepayModel
	Env          structured.RateEnv
	TrancheIndex int
	OAS          float64
}

// StructuredProvider resolves an instrument's structured-product spec as of a
// point in time. ok=false ⇒ not a structured product (excluded from the
// measures).
type StructuredProvider interface {
	Structured(ctx context.Context, instrumentID string, asOf time.Time) (StructuredSpec, bool)
}

// RegisterStructuredRisk registers StructDuration/StructConvexity/StructWAL on r,
// each closing over ctx + the provider. Call at engine startup after
// DefaultRegistry; tests register against a deterministic provider.
func RegisterStructuredRisk(ctx context.Context, r *Registry, provider StructuredProvider) {
	r.Register(MeasureStructDuration, structMeasure(ctx, MeasureStructDuration, provider))
	r.Register(MeasureStructConvexity, structMeasure(ctx, MeasureStructConvexity, provider))
	r.Register(MeasureStructWAL, structMeasure(ctx, MeasureStructWAL, provider))
}

// structMeasure builds the MeasureFunc for one structured measure — a |MV|-
// weighted average across structured positions.
func structMeasure(ctx context.Context, name v1.MeasureName, provider StructuredProvider) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		asOf := p.AsOf()
		var weighted, weight float64
		for _, pos := range p.Positions() {
			if pos.MarketValue == nil {
				continue
			}
			spec, ok := provider.Structured(ctx, string(pos.InstrumentID), asOf)
			if !ok {
				continue
			}
			mv := decimalToFloat(pos.MarketValue.GetAmount())
			if mv < 0 {
				mv = -mv
			}
			var val float64
			switch name {
			case MeasureStructWAL:
				val = spec.Deal.Project(spec.Prepay, spec.Env).Tranches[spec.TrancheIndex].WAL()
			case MeasureStructConvexity:
				_, val = structured.EffectiveRisk(spec.Deal, spec.Prepay, spec.Env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
			default: // MeasureStructDuration
				val, _ = structured.EffectiveRisk(spec.Deal, spec.Prepay, spec.Env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
			}
			weighted += mv * val
			weight += mv
		}
		ratio := 0.0
		if weight != 0 {
			ratio = weighted / weight
		}
		return v1.Measure{Name: name, Value: floatToDecimal(ratio, structRiskExp)}
	}
}
