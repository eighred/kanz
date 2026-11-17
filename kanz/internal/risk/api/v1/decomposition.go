package v1

import (
	"errors"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// These are response bounds, not truncation thresholds. An oversized book or
// factor catalogue refuses the entire query; no omitted contribution is relabelled
// as a residual. Keys are bounded too, including the exclusion sample.
const (
	MaxDecompositionInstruments = 1024
	MaxDecompositionFactors     = 64
	MaxDecompositionKeyBytes    = 256
	MaxDecompositionParams      = 32
)

var (
	ErrDecompositionUnavailable = errors.New("risk: decomposition inputs unavailable")
	ErrDecompositionTooLarge    = errors.New("risk: decomposition response bound exceeded")
)

// DecomposeRiskRequest selects current state only. A nonzero AsOf is refused:
// retained scalar evaluations do not retain this vector's complete inputs.
type DecomposeRiskRequest struct {
	PortfolioID PortfolioID
	AsOf        time.Time
}

type FactorRiskContribution struct {
	Key       string
	Exposure  *commonpb.Decimal // B^T v; portfolio base-currency exposure
	Component *commonpb.Decimal // Euler contribution in base-currency risk units
}

type InstrumentRiskContribution struct {
	InstrumentID InstrumentID
	Marginal     *commonpb.Decimal // risk units per unit of signed market value
	Component    *commonpb.Decimal // signed market value times marginal risk
}

// RiskDecomposition is over one frozen book and one fitted model. Components are
// projected from each computed float's shortest round-trip decimal representation
// using dec.ParseProtoExact. An unrepresentable value refuses the query; there is
// no additional fixed-scale rounding or balancing plug. This preserves the
// computed observation, not an assertion of exact real-number linear algebra.
// A nonempty Coverage exclusion count makes the numbers incomplete, not zero risk.
type RiskDecomposition struct {
	Total, Systematic, Specific, SpecificComponent *commonpb.Decimal
	ByFactor                                       []FactorRiskContribution
	ByInstrument                                   []InstrumentRiskContribution
	Coverage                                       InputCoverage
	Provenance                                     MeasureProvenance
}

type DecomposeRiskResponse struct {
	PortfolioID    PortfolioID
	AsOf           time.Time
	CurrencyCode   string
	Decomposition  RiskDecomposition
	QualityFlags   []QualityFlag
	SourcePosition *commonpb.LogPosition
}
