// Package library is the MODEL-01h named-scenario catalog (RISK-09 extension).
// It turns curated stress narratives — historical crises and hypothetical curve
// shifts — into the api/v1 shock slices the scenario engine already evaluates,
// so a desk runs "what would 2008 do to my book?" without hand-assembling
// shocks.
//
// # Why scenarios are sector curves, not parallel shifts
//
// A crisis is not uniform: in 2008 financials fell far harder than staples; in
// the COVID drawdown energy and travel collapsed while staples held. A single
// ParallelShift cannot express that dispersion. Each named scenario here is a
// SECTOR CURVE — a SectorShock per GICS sector (MODEL-01h's new shock type,
// resolved against the MODEL-01f factor model) — so the differentiated impact
// is preserved.
//
// # Which means every scenario here needs a classifier, and there is none
//
// A position whose sector is OUTSIDE the curve is left unshocked, and that is
// the honest behavior: the scenario only claims to know the sectors it names.
// This doc used to put "unclassified" in the same sentence, which was the one
// thing it could not claim. An instrument nobody can classify is not evidence of
// a sector the curve does not cover — and since no factor.Classifier is
// constructed at any composition root, EVERY instrument was in that state, so
// every scenario in this catalog returned the input book (#640). The engine now
// refuses instead: see v1.ErrScenarioUnresolvable.
//
// # The magnitudes are illustrative, point-in-time stress inputs
//
// The per-sector returns approximate the historical drawdowns (2008 calendar
// year; the Feb–Mar 2020 COVID peak-to-trough) at GICS-sector granularity.
// They are stress-test inputs, not a backtest data source (that is the MODEL-01b
// price plane) — a quant recalibrates them, but the curated defaults make the
// catalog useful out of the box.
package library

import (
	"sort"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// gicsTaxonomy is the classification taxonomy the named scenarios shock against
// — matches factor.Sector.Taxonomy / reference.v1.SectorClassification.
const gicsTaxonomy = "GICS"

// GICS sector codes — the eleven top-level sectors, the curve's tenors.
const (
	gicsEnergy                = "10"
	gicsMaterials             = "15"
	gicsIndustrials           = "20"
	gicsConsumerDiscretionary = "25"
	gicsConsumerStaples       = "30"
	gicsHealthCare            = "35"
	gicsFinancials            = "40"
	gicsInformationTechnology = "45"
	gicsCommunicationServices = "50"
	gicsUtilities             = "55"
	gicsRealEstate            = "60"
)

// pct builds a fractional Decimal at 1% granularity: pct(-55) == -0.55, the
// "drop this sector 55%" input ShockMoney reads as ×(1−0.55).
func pct(wholePercent int64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: wholePercent, Exponent: -2}
}

// GlobalFinancialCrisis2008 is the 2008 GFC sector curve — approximate GICS
// calendar-2008 total returns, financials the epicenter.
func GlobalFinancialCrisis2008() []v1.ScenarioShock {
	return SectorCurve(gicsTaxonomy, map[string]*commonpb.Decimal{
		gicsEnergy:                pct(-34),
		gicsMaterials:             pct(-46),
		gicsIndustrials:           pct(-39),
		gicsConsumerDiscretionary: pct(-33),
		gicsConsumerStaples:       pct(-15),
		gicsHealthCare:            pct(-23),
		gicsFinancials:            pct(-55),
		gicsInformationTechnology: pct(-43),
		gicsCommunicationServices: pct(-30),
		gicsUtilities:             pct(-29),
		gicsRealEstate:            pct(-42),
	})
}

// Covid2020Crash is the Feb–Mar 2020 COVID drawdown sector curve — approximate
// GICS peak-to-trough, energy the epicenter.
func Covid2020Crash() []v1.ScenarioShock {
	return SectorCurve(gicsTaxonomy, map[string]*commonpb.Decimal{
		gicsEnergy:                pct(-55),
		gicsMaterials:             pct(-37),
		gicsIndustrials:           pct(-40),
		gicsConsumerDiscretionary: pct(-35),
		gicsConsumerStaples:       pct(-25),
		gicsHealthCare:            pct(-28),
		gicsFinancials:            pct(-42),
		gicsInformationTechnology: pct(-32),
		gicsCommunicationServices: pct(-30),
		gicsUtilities:             pct(-36),
		gicsRealEstate:            pct(-42),
	})
}

// VolSpikeRiskOff is the canonical option-book stress (DERIV-01e): a broad
// equity drawdown paired with an implied-vol spike — the regime where a
// long-gamma book's convexity and a short-vol book's vega losses both bite,
// which a price-only curve cannot express. The ParallelShift drives the spot
// drop on the linear path; the global VolShock only takes effect under full
// revaluation (scenario.WithRevaluer / EvaluateReval), where option positions
// are repriced.
//
// SO IT IS REFUSED, NOT PARTIALLY SERVED, ON A DEPLOYMENT WITH NO REVALUER —
// and that is the whole reason the shocks are paired here. Applying only the
// spot leg is not a conservative approximation of this stress, it is a
// different stress: it tells a short-vol desk that the regime built to hurt it
// costs it the delta and nothing else (#1035).
//
// Unlike the GICS-curve scenarios it is not in the Named/Names catalog (those
// are sector curves by construction); a caller passes it directly.
func VolSpikeRiskOff() []v1.ScenarioShock {
	return []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(-20)},                                        // −20% across the book
		v1.VolShock{AbsBump: &commonpb.Decimal{Coefficient: 15, Exponent: -2}}, // +15 vol points, all underlyings
	}
}

// SectorCurve builds the hypothetical curve-shift primitive: one SectorShock per
// (sectorCode → pct) entry, all under taxonomy. This is what the named
// scenarios are built from and the seam a caller uses for a custom curve (e.g.
// a 100bp-equivalent steepener across sectors). nil/zero-pct entries are
// dropped — a curve point with no shift is not a shock. Output order is stable
// (sorted by sector code) for deterministic audit/replay.
func SectorCurve(taxonomy string, shifts map[string]*commonpb.Decimal) []v1.ScenarioShock {
	codes := make([]string, 0, len(shifts))
	for code := range shifts {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	out := make([]v1.ScenarioShock, 0, len(codes))
	for _, code := range codes {
		p := shifts[code]
		if p == nil || p.Coefficient == 0 {
			continue
		}
		out = append(out, v1.SectorShock{Taxonomy: taxonomy, Code: code, Pct: p})
	}
	return out
}

// Named looks up a scenario by its catalog name (case-sensitive), returning the
// shocks and true, or nil and false when unknown. The dispatch seam an API uses
// to drive Engine.EvaluateScenario from a scenario name.
//
// THE CALLER IS grpcsrv.namedScenario, WHICH RESOLVES query.v1's scenario_name
// SERVER-SIDE. This comment described that seam for as long as the package
// existed and nothing read it: the shocks these scenarios are made of
// (v1.SectorShock) had no member in the query.v1 ScenarioShock oneof, so no
// caller of any kind could ask for a named scenario and the whole catalog was
// unreachable (#1004). Keeping the resolution on the server is what makes the
// magnitudes in this file ONE versionable definition rather than a copy per
// client.
func Named(name string) ([]v1.ScenarioShock, bool) {
	b, ok := catalog[name]
	if !ok {
		return nil, false
	}
	return b(), true
}

// Names returns the catalog's scenario names in stable order.
func Names() []string {
	out := make([]string, 0, len(catalog))
	for name := range catalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// catalog is the name→builder registry the Named/Names dispatch reads. Builders
// (not pre-built slices) so each lookup hands back a fresh, caller-owned slice.
var catalog = map[string]func() []v1.ScenarioShock{
	"GFC_2008":   GlobalFinancialCrisis2008,
	"COVID_2020": Covid2020Crash,
}
