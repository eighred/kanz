// Package factor is the risk engine's factor / classification layer (MODEL-01f).
// It maps instruments to the factor dimensions a portfolio's risk decomposes
// along — primarily industry sector, plus asset class — and uses that mapping
// to complete the RISK-06 ExposureBySector deferral (which was waiting for
// exactly this instrument→sector reference table).
//
// # The classifier is the reference-data mirror of the returns provider
//
// Just as MODEL-01c's compute.ReturnsProvider reads the MODEL-01b price store,
// the Classifier reads instrument reference data (reference.v1.InstrumentReference
// — sector, asset_class). Classification is slowly-changing and point-in-time:
// a sector reclassification carries an effective time, so a backtest reads the
// mapping that was in effect then (the reference.v1 as_of SCD discipline), which
// is why Classify takes an asOf. The production source is internal/refdata,
// which projects datamaster's golden security master (#640) — the reference
// mirror of MODEL-01b, and the thing this doc called "not yet built" for as long
// as it was. StaticClassifier is now a TEST fixture only.
//
// ONE LIMIT OF THE REAL SOURCE, stated here because this doc is where the asOf
// discipline is promised: the master keeps ONE snapshot per instrument, not an
// SCD history. refdata refuses a question about a time before the snapshot's own
// as_of rather than answering it with today's classification, so a backtest gets
// a refusal instead of a reclassification that had not happened. Closing that
// properly is the master's work, not the cache's.
//
// # Factor model scope
//
// Classification carries both factor dimensions an instrument loads on (sector,
// asset class). Sector is consumed now (exposure bucketing). The same mapping is
// the seam a reduced-rank factor covariance would plug into to replace MODEL-01e's
// full empirical covariance — a future enhancement, so no covariance machinery
// lives here yet.
package factor

import (
	"context"
	"time"
)

// Sector is an instrument's industry classification — taxonomy + code, mirroring
// reference.v1.SectorClassification. Key() is the exposure-bucket string the
// domain layer uses (e.g. "GICS:45"), built from the two parts without parsing.
type Sector struct {
	Taxonomy string
	Code     string
}

// Key returns the "TAXONOMY:CODE" bucket string, or "" when the sector is unset.
func (s Sector) Key() string {
	if s.IsZero() {
		return ""
	}
	return s.Taxonomy + ":" + s.Code
}

// IsZero reports whether the sector is unset (no classification).
func (s Sector) IsZero() bool { return s.Taxonomy == "" && s.Code == "" }

// Classification is the factor-relevant reference data for one instrument.
type Classification struct {
	// Sector is the industry classification — the SECTOR exposure factor.
	Sector Sector
	// AssetClass is the reference.v1 asset class (e.g. "EQUITY") — the second
	// factor dimension. Carried for the future reduced-rank factor covariance;
	// not yet consumed by exposure.
	AssetClass string
}

// Classifier is the instrument→factor lookup. Point-in-time via asOf so a
// historical recompute/backtest reads the classification in effect then.
type Classifier interface {
	// Classify returns the instrument's classification as of asOf. ok=false when
	// the instrument is unknown (the caller buckets it as unclassified rather
	// than dropping it, so totals reconcile).
	Classify(ctx context.Context, instrumentID string, asOf time.Time) (Classification, bool)
}

// StaticClassifier is an in-memory Classifier backed by a fixed instrument→
// classification map (asOf-independent).
//
// TESTS ARE ITS ONLY CONSTRUCTOR AND THAT IS NOW CORRECT RATHER THAN A GAP. It
// once carried "the engine loads it from reference data", describing a
// composition root that had never existed; the production source is
// refdata.Cache.Factor (#640), which services/risk-engine passes to
// engine.WithClassifier. A composition root reaching for this type instead
// would make the whole named-scenario catalog report confident numbers derived
// from a map somebody typed, which #345 rules out.
type StaticClassifier map[string]Classification

// Classify implements Classifier; asOf is ignored (the static map is a single
// point-in-time snapshot).
func (c StaticClassifier) Classify(_ context.Context, instrumentID string, _ time.Time) (Classification, bool) {
	cl, ok := c[instrumentID]
	return cl, ok
}

// Compile-time assertion that StaticClassifier satisfies Classifier.
var _ Classifier = StaticClassifier(nil)
