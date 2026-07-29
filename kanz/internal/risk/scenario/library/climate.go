package library

import (
	"sort"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// CLIMATE-01c (risk side) — named climate-transition scenarios as GICS sector
// curves, joining the GFC/COVID catalog. A disorderly net-zero transition reprices
// carbon-intensive sectors (energy, materials, utilities) hard while sparing
// low-carbon ones — a dispersion a parallel shift cannot express, exactly the
// reason the named scenarios are sector curves (library.go). This is the
// engine-native climate scenario at SECTOR granularity; the finer carbon-intensity-
// driven climate-VaR (per issuer, PCAF-style) lives in internal/sustainability,
// which expands a carbon price against each issuer's emissions — the two are the
// coarse and fine views of the same transition risk. The library stays pure
// (no sustainability import): it shocks sectors the engine already classifies.

// DisorderlyTransition is a late, abrupt net-zero transition: carbon-intensive
// sectors reprice sharply as a carbon price arrives unanticipated, financials and
// real estate take second-order hits, low-carbon sectors are roughly spared.
func DisorderlyTransition() []v1.ScenarioShock {
	return SectorCurve(gicsTaxonomy, map[string]*commonpb.Decimal{
		gicsEnergy:                pct(-45),
		gicsMaterials:             pct(-30),
		gicsUtilities:             pct(-25),
		gicsIndustrials:           pct(-15),
		gicsConsumerDiscretionary: pct(-8),
		gicsFinancials:            pct(-10),
		gicsRealEstate:            pct(-12),
	})
}

// HotHousePhysical is a hot-house world: little transition, so carbon-intensive
// sectors are spared the transition shock, but physical climate damage hits
// real-asset-heavy sectors (real estate, utilities, energy infrastructure) and
// agriculture-exposed materials/staples.
func HotHousePhysical() []v1.ScenarioShock {
	return SectorCurve(gicsTaxonomy, map[string]*commonpb.Decimal{
		gicsRealEstate:      pct(-25),
		gicsUtilities:       pct(-18),
		gicsEnergy:          pct(-15),
		gicsMaterials:       pct(-12),
		gicsConsumerStaples: pct(-10),
	})
}

// NamedClimateScenario looks up a climate scenario by catalog name, returning the
// sector-curve shocks and true, or nil and false when unknown.
func NamedClimateScenario(name string) ([]v1.ScenarioShock, bool) {
	b, ok := climateCatalog[name]
	if !ok {
		return nil, false
	}
	return b(), true
}

// ClimateScenarioNames returns the climate catalog names in stable order.
func ClimateScenarioNames() []string {
	out := make([]string, 0, len(climateCatalog))
	for name := range climateCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var climateCatalog = map[string]func() []v1.ScenarioShock{
	"DISORDERLY_TRANSITION": DisorderlyTransition,
	"HOT_HOUSE_PHYSICAL":    HotHousePhysical,
}
