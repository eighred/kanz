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
// is preserved. Positions whose instrument is unclassified or whose sector is
// outside the curve are left unshocked (the engine no-ops them), which is the
// honest behavior: the scenario only claims to know the sectors it names.
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

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
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
// shocks and true, or nil and false when unknown. The dispatch seam an API or
// CLI uses to drive Engine.EvaluateScenario from a scenario name.
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
