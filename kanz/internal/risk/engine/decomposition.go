package engine

import (
	"context"
	"fmt"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
)

// WithFactorModel shares the composition root's existing provider. It does not
// fit another model or make scalar-registry entries out of vector quantities.
func WithFactorModel(provider compute.ModelProvider) EngineOption {
	return func(e *EngineImpl) { e.factorModel = provider }
}

func (e *EngineImpl) DecomposeRisk(ctx context.Context, req v1.DecomposeRiskRequest) (v1.DecomposeRiskResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.DecomposeRiskResponse{}, err
	}
	if req.PortfolioID == "" {
		return v1.DecomposeRiskResponse{}, v1.ErrInvalidRequest
	}
	if !req.AsOf.IsZero() {
		return v1.DecomposeRiskResponse{}, fmt.Errorf("%w: historical decomposition is unavailable; live state cannot answer as_of", v1.ErrInvalidRequest)
	}
	if err := e.refuseIfNotOwned(req.PortfolioID); err != nil {
		return v1.DecomposeRiskResponse{}, err
	}
	p, ok := e.store.Snapshot(req.PortfolioID)
	if !ok {
		return v1.DecomposeRiskResponse{}, v1.ErrPortfolioNotFound
	}
	d, err := compute.DecomposePortfolio(ctx, p, e.factorModel)
	if err != nil {
		return v1.DecomposeRiskResponse{}, err
	}
	_, flags := e.detector.Assess(p.AsOf())
	if d.Coverage.ExcludedCount != 0 {
		flags = append(flags, v1.QualityFlagInputsUnresolved)
	}
	return v1.DecomposeRiskResponse{
		PortfolioID: req.PortfolioID, AsOf: p.AsOf(), CurrencyCode: string(p.BaseCurrency()),
		Decomposition: d, QualityFlags: flags, SourcePosition: p.LogPosition(),
	}, nil
}
