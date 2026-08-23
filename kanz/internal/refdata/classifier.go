package refdata

import (
	"context"
	"time"

	"github.com/eighred/kanz/internal/compliance"
)

// THE TWO PROJECTIONS, AND WHY ONLY ONE OF THEM IS HERE.
//
// compliance.Classifier and factor.Classifier are the same question in two
// vocabularies. They are separate types because internal/compliance and
// internal/risk/compute/factor are separate subsystems and neither may import
// the other for a struct — not because the estate has, or should have, two
// reference-data sources. Both read ONE Cache, so a sector resolved for a
// mandate and a sector resolved for a stress shock are the same string from the
// same golden record, resolved at the same instant.
//
// THE FACTOR PROJECTION IS NOT IN THIS FILE, and that is the RISK-02 module
// boundary rather than a preference: test/arch/risk_boundary_test.go permits
// only internal/risk/api/v* to be imported from outside the risk module, and
// exempts exactly one consumer — the risk-engine service, as the module's
// composition root. So the factor.Classifier adapter lives there, in
// services/risk-engine/internal/app, where the wiring layer is already entitled
// to know both types. It is still ONE adapter with one caller; what moved is
// which package is allowed to name factor.Classification.

// Compliance returns the cache as a compliance.Classifier — the issuer, sector
// and asset-class lookup behind the ISSUER, SECTOR and ASSET_CLASS mandate
// dimensions.
func (c *Cache) Compliance() compliance.Classifier { return complianceView{c} }

type complianceView struct{ c *Cache }

// Classify implements compliance.Classifier.
//
// ok=false IS THE REFUSAL, and every path to it matters: the instrument is not
// resolved yet (cold), its record has aged past MaxAge (stale), or the master
// does not hold it. compliance.unresolvedDimension turns any of them into a
// violation naming the holdings, so none of them can be mistaken for a pass.
//
// AN EMPTY FIELD IS STILL RETURNED WITH ok=true, deliberately. The record is
// genuinely resolved — the master says this instrument has no issuer, which is
// the true answer for FX and a broad index — and it is bucketKey's empty string
// that unresolvedDimension keys the per-instrument refusal on. Returning
// ok=false here instead would say "not resolved" about an instrument that IS
// resolved, and send an operator looking for missing reference data that is not
// missing.
func (v complianceView) Classify(_ context.Context, instrumentID string, asOf time.Time) (compliance.Attributes, bool) {
	rec, ok := v.c.Lookup(instrumentID, asOf)
	if !ok {
		return compliance.Attributes{}, false
	}
	return compliance.Attributes{
		Issuer:     rec.IssuerID,
		Sector:     rec.Sector.Key(),
		AssetClass: rec.AssetClass,
	}, true
}

// Compile-time assertion that the projection satisfies the interface it exists
// for. Without it, a signature change in compliance.Classifier would fail at
// two composition roots rather than at the type that broke.
var _ compliance.Classifier = complianceView{}
