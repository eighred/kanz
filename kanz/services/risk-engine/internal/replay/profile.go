package replay

import (
	"context"
	"errors"
	"slices"
	"strconv"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

// Profile is the retained composition of the production risk calculator. It
// carries model parameters, not environment variables or database credentials.
// A new family must acquire a replay codec before this profile accepts it.
type Profile struct {
	Version          int
	Names            []v1.MeasureName
	Historical       bool
	HistoricalConfig varmodel.Config
	Factors          bool
	FixedIncome      bool
	Structured       bool
	Liquidity        bool
	LiquidityModel   liquidity.Model
	Margin           bool
	Sector           bool
}

func productionProfile(registry *compute.Registry, sector bool) (Profile, error) {
	names := registry.Names()
	has := func(n v1.MeasureName) bool { return slices.Contains(names, n) }
	p := Profile{
		Version: 1, Names: names, Sector: sector,
		Historical:       has(compute.MeasureES99),
		HistoricalConfig: varmodel.Config{Confidence: varmodel.DefaultConfidence},
		Factors:          has(compute.MeasureFactorVaR99), FixedIncome: has(compute.MeasureDV01),
		Structured: has(compute.MeasureStructWAL), Liquidity: has(compute.MeasureLiquidationHorizon),
		LiquidityModel: liquidity.DefaultModel(), Margin: has(compute.MeasureLiquidationProximity),
	}
	if p.Historical {
		parameters, ok := registry.Parameters(compute.MeasureVaR99)
		if !ok {
			return p, errors.New("historical risk registration lacks reproduction parameters")
		}
		confidence, err := strconv.ParseFloat(parameters["confidence"], 64)
		if err != nil {
			return p, err
		}
		window, err := strconv.Atoi(parameters["window"])
		if err != nil {
			return p, err
		}
		p.HistoricalConfig = varmodel.Config{Confidence: confidence, Window: window}
	}
	if p.Liquidity {
		parameters, ok := registry.Parameters(compute.MeasureLiquidationHorizon)
		if !ok {
			return p, errors.New("liquidity registration lacks reproduction parameters")
		}
		participation, err := strconv.ParseFloat(parameters["participation_rate"], 64)
		if err != nil {
			return p, err
		}
		impact, err := strconv.ParseFloat(parameters["impact_coefficient"], 64)
		if err != nil {
			return p, err
		}
		p.LiquidityModel = liquidity.Model{ParticipationRate: participation, ImpactCoeff: impact}
	}
	_, err := p.registry()
	return p, err
}

func (p Profile) registry() (*compute.Registry, error) {
	if p.Version != 1 {
		return nil, errors.New("unsupported retained risk calculator version")
	}
	ctx := context.Background()
	r := compute.DefaultRegistry()
	if p.Historical {
		varmodel.Register(ctx, r, Returns{}, p.HistoricalConfig)
	}
	if p.Factors {
		compute.RegisterFactorRisk(ctx, r, compute.FactorProviders{Model: Models{}})
	}
	if p.FixedIncome {
		compute.RegisterFIRisk(ctx, r, compute.FIProviders{Terms: Bonds{}, Curve: Curves{}})
	}
	if p.Structured {
		compute.RegisterStructuredRisk(ctx, r, compute.StructuredProviders{Terms: Structures{}, Curve: Curves{}})
	}
	if p.Liquidity {
		compute.RegisterLiquidityRisk(ctx, r, Liquidity{}, p.LiquidityModel, nil)
	}
	if p.Margin {
		compute.RegisterMarginRisk(ctx, r, Margins{})
	}
	if !slices.Equal(r.Names(), p.Names) {
		return nil, errors.New("risk registry contains a family without a retained replay contract")
	}
	return r, nil
}
