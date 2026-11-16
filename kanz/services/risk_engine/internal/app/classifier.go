package app

import (
	"context"
	"time"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/internal/risk/compute/factor"
)

// THE FACTOR CLASSIFIER'S ADAPTER LIVES IN THE COMPOSITION ROOT (#640).
//
// internal/refdata is the estate's one instrument reference-data source and it
// projects itself as a compliance.Classifier directly. It cannot do the same
// for factor.Classifier: test/arch/risk_boundary_test.go permits only
// internal/risk/api/v* to be imported from outside the risk module, and exempts
// exactly one consumer — this service, as the module's composition root. So the
// eight lines that rename refdata's vocabulary into the factor package's live
// here, in the layer that is already entitled to know both.
//
// THIS IS NOT A SECOND SOURCE. It reads the same *refdata.Cache the compliance
// gate reads, so a sector a mandate buckets on and a sector a stress shocks are
// the same string, resolved from the same golden record at the same instant.
// The day a second adapter appears anywhere, that property is gone and nothing
// will report the disagreement.

// FactorClassifier adapts the shared reference-data cache to the risk module's
// factor.Classifier — the lookup behind MODEL-01h sector shocks and the RISK-06
// SECTOR exposure dimension.
type FactorClassifier struct{ cache *refdata.Cache }

// NewFactorClassifier wraps the cache. A nil cache yields a nil Classifier
// rather than one that answers "unknown" to everything: engine.WithClassifier
// treats nil as "this deployment has no reference-data source", which is what
// makes a scenario refuse with ErrScenarioUnresolvable instead of quietly
// shocking nothing.
func NewFactorClassifier(cache *refdata.Cache) factor.Classifier {
	if cache == nil {
		return nil
	}
	return FactorClassifier{cache: cache}
}

// Classify implements factor.Classifier.
//
// Sector crosses as the structured factor.Sector rather than as a joined key,
// because factor.Sector.Key is what the exposure buckets on and re-splitting a
// joined string would be a second parser for one format.
//
// ok=false means the cache cannot answer — cold, stale past its bound, or an
// instrument the master does not hold. SectorExposure buckets those under
// factor.UnclassifiedSector so the totals still reconcile, and EvaluateScenario
// refuses rather than returning an unshocked position.
func (f FactorClassifier) Classify(_ context.Context, instrumentID string, asOf time.Time) (factor.Classification, bool) {
	rec, ok := f.cache.Lookup(instrumentID, asOf)
	if !ok {
		return factor.Classification{}, false
	}
	return factor.Classification{
		Sector:     factor.Sector{Taxonomy: rec.Sector.Taxonomy, Code: rec.Sector.Code},
		AssetClass: rec.AssetClass,
	}, true
}

var _ factor.Classifier = FactorClassifier{}
