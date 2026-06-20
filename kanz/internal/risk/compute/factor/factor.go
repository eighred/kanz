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
// is why Classify takes an asOf. The production source is a reference-data store
// (the reference mirror of MODEL-01b, not yet built); StaticClassifier is the
// in-memory stand-in the engine loads from reference state/events meanwhile.
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
// classification map (asOf-independent). The engine loads it from reference
// data; tests construct it directly.
type StaticClassifier map[string]Classification

// Classify implements Classifier; asOf is ignored (the static map is a single
// point-in-time snapshot).
func (c StaticClassifier) Classify(_ context.Context, instrumentID string, _ time.Time) (Classification, bool) {
	cl, ok := c[instrumentID]
	return cl, ok
}

// Compile-time assertion that StaticClassifier satisfies Classifier.
var _ Classifier = StaticClassifier(nil)
