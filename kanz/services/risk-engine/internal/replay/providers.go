package replay

import (
	"context"
	"errors"
	"time"

	marketreturns "github.com/eighred/kanz/internal/marketdata/returns"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/liquidity"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
)

type Returns struct{ Source compute.ReturnsProvider }

func (r Returns) Returns(ctx context.Context, id string, at time.Time, window int) ([]float64, error) {
	a := resolve(ctx, inputKey{Kind: "returns-legacy", ID: id, AsOf: at, Window: window}, func() answer[[]float64] {
		if r.Source == nil {
			return answer[[]float64]{Error: "returns provider unavailable"}
		}
		v, err := r.Source.Returns(ctx, id, at, window)
		out := answer[[]float64]{Value: v}
		if err != nil {
			out.Error = "returns provider failed"
		}
		return out
	})
	if a.Error != "" {
		return nil, errors.New(a.Error)
	}
	return a.Value, nil
}

func (r Returns) DatedReturns(ctx context.Context, id string, at time.Time, window int) (marketreturns.Series, error) {
	a := resolve(ctx, inputKey{Kind: "returns-dated", ID: id, AsOf: at, Window: window}, func() answer[marketreturns.Series] {
		source, ok := r.Source.(marketreturns.DatedProvider)
		if !ok {
			return answer[marketreturns.Series]{Error: "dated returns provider unavailable"}
		}
		v, err := source.DatedReturns(ctx, id, at, window)
		out := answer[marketreturns.Series]{Value: v}
		if err != nil {
			out.Error = "dated returns provider failed"
		}
		return out
	})
	if a.Error != "" {
		return marketreturns.Series{}, errors.New(a.Error)
	}
	return a.Value, nil
}

type Models struct{ Source compute.ModelProvider }

func (r Models) Model(ctx context.Context, at time.Time) (*factormodel.Model, bool) {
	a := resolve(ctx, inputKey{Kind: "factor-model", AsOf: at}, func() answer[*factorpb.FactorModelSnapshot] {
		if r.Source == nil {
			return answer[*factorpb.FactorModelSnapshot]{}
		}
		m, ok := r.Source.Model(ctx, at)
		if !ok || m == nil {
			return answer[*factorpb.FactorModelSnapshot]{}
		}
		return answer[*factorpb.FactorModelSnapshot]{Value: publish.ToProtoFactorModelSnapshot(m), Known: true}
	})
	if !a.Known {
		return nil, false
	}
	m, err := factormodel.FromSnapshot(a.Value)
	if err != nil {
		failInput(ctx, err)
	}
	return m, err == nil
}

type Curves struct{ Source compute.CurveProvider }

func (r Curves) Curve(ctx context.Context, currency string, at time.Time) (*curve.Curve, bool) {
	a := resolve(ctx, inputKey{Kind: "resolved-curve", ID: currency, AsOf: at}, func() answer[*domainpb.CalibratedCurve] {
		if r.Source == nil {
			return answer[*domainpb.CalibratedCurve]{}
		}
		c, ok := r.Source.Curve(ctx, currency, at)
		if !ok || c == nil {
			return answer[*domainpb.CalibratedCurve]{}
		}
		// This is the resolved input at the lookup cutoff, not a new calibration
		// FACT. The original calibration is retained independently by its producer.
		return answer[*domainpb.CalibratedCurve]{Value: publish.ToProtoCalibratedCurve(currency, at, c), Known: true}
	})
	if !a.Known {
		return nil, false
	}
	c, err := curve.FromArtifact(a.Value)
	if err != nil {
		failInput(ctx, err)
	}
	return c, err == nil
}

type Bonds struct{ Source compute.BondTermsProvider }

func (r Bonds) BondTerms(ctx context.Context, id string, at time.Time) (compute.BondSpec, compute.TermsResolution) {
	a := resolve(ctx, inputKey{Kind: "bond-terms", ID: id, AsOf: at}, func() answer[compute.BondSpec] {
		if r.Source == nil {
			return answer[compute.BondSpec]{}
		}
		v, status := r.Source.BondTerms(ctx, id, at)
		return answer[compute.BondSpec]{Value: v, Status: int(status)}
	})
	return a.Value, compute.TermsResolution(a.Status)
}

type Structures struct {
	Source compute.StructuredProvider
}

func (r Structures) Structured(ctx context.Context, id string, at time.Time) (compute.StructuredSpec, compute.TermsResolution) {
	a := resolve(ctx, inputKey{Kind: "structured-terms", ID: id, AsOf: at}, func() answer[structuredInput] {
		if r.Source == nil {
			return answer[structuredInput]{}
		}
		v, status := r.Source.Structured(ctx, id, at)
		stored, err := encodeStructure(v)
		if err != nil {
			failInput(ctx, err)
			return answer[structuredInput]{}
		}
		return answer[structuredInput]{Value: stored, Status: int(status)}
	})
	spec, err := a.Value.decode()
	if err != nil {
		failInput(ctx, err)
		return compute.StructuredSpec{}, compute.TermsUnusable
	}
	return spec, compute.TermsResolution(a.Status)
}

type Liquidity struct {
	Source liquidity.Provider
	Spread bool
}

func (r Liquidity) ServesSpread() bool { return r.Spread }

func (r Liquidity) Liquidity(ctx context.Context, id string, at time.Time) (liquidity.LiquiditySpec, bool) {
	a := resolve(ctx, inputKey{Kind: "liquidity", ID: id, AsOf: at}, func() answer[liquidity.LiquiditySpec] {
		if r.Source == nil {
			return answer[liquidity.LiquiditySpec]{}
		}
		v, ok := r.Source.Liquidity(ctx, id, at)
		return answer[liquidity.LiquiditySpec]{Value: v, Known: ok}
	})
	return a.Value, a.Known
}

type Margins struct{ Source compute.MarginProvider }

func (r Margins) AccountMargins(ctx context.Context, id v1.PortfolioID) ([]compute.AccountMargin, bool) {
	a := resolve(ctx, inputKey{Kind: "account-margins", ID: string(id)}, func() answer[[]compute.AccountMargin] {
		if r.Source == nil {
			return answer[[]compute.AccountMargin]{}
		}
		v, ok := r.Source.AccountMargins(ctx, id)
		return answer[[]compute.AccountMargin]{Value: v, Known: ok}
	})
	return a.Value, a.Known
}

type Classifier struct{ Source factor.Classifier }

func (r Classifier) Classify(ctx context.Context, id string, at time.Time) (factor.Classification, bool) {
	a := resolve(ctx, inputKey{Kind: "classification", ID: id, AsOf: at}, func() answer[factor.Classification] {
		if r.Source == nil {
			return answer[factor.Classification]{}
		}
		v, ok := r.Source.Classify(ctx, id, at)
		return answer[factor.Classification]{Value: v, Known: ok}
	})
	return a.Value, a.Known
}
