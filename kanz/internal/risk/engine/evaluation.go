package engine

import (
	"context"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Evaluation contains the outputs and source coordinate of one frozen book.
type Evaluation struct {
	Exposure       *domain.ExposureSet
	Measures       *domain.MeasureSet
	SourcePosition *commonpb.LogPosition
}

// Evaluator is the durable evaluation boundary supplied by the service. A
// successful Compute means both inputs and outputs are retained. Replay may
// only read retained inputs; a missing input must never use a current provider.
type Evaluator interface {
	Compute(context.Context, *domain.Portfolio, factor.Classifier) (Evaluation, error)
	Replay(context.Context, v1.PortfolioID, time.Time) (Evaluation, error)
}

func WithEvaluator(evaluator Evaluator) EngineOption {
	return func(e *EngineImpl) { e.evaluator = evaluator }
}

func WithRecomputeEvaluator(evaluator Evaluator) RecomputerOption {
	return func(r *Recomputer) { r.evaluator = evaluator }
}
