package app

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
)

// WHICH OF THE MEASURES THIS PLATFORM IMPLEMENTS DOES THIS ENGINE ACTUALLY
// SERVE (#509)?
//
// # The failure this makes visible
//
// internal/risk/engine.filterMeasures drops unknown names — its own doc says so.
// So a client that asks for DV01 on a bond book gets 200 with DV01 absent, and
// that is INDISTINGUISHABLE from three other things: a portfolio holding no
// bonds, a computation that ran and yielded nothing, and a measure this build
// never registered at all. Only the last is a defect, and it was the invisible
// one.
//
// When this landed the engine served EIGHT of twenty-six catalogued measures.
// The eighteen dark ones were every Greek, every fixed-income measure, the whole
// factor, liquidity, structured and XVA families — implemented, unit-tested,
// several of them benchmarked under #471's "every analytic serves production
// unvalidated", and none of them reachable by any client. Nothing said so.
//
// # Why a gauge per measure rather than a coverage ratio
//
// A ratio answers "how much" and hides "which". 8/26 is the same number whether
// the missing eighteen are the exotic tail or the entire fixed-income book, and
// the difference is what an operator needs. The dark ones carry their family as
// a label so a dashboard groups them by the single seam that would light them
// up — eighteen dark measures are four missing wirings, not eighteen problems.
//
// # Why one-shot, unlike AnalyticsPosture
//
// AnalyticsPosture is a Collector because a validation EXPIRES — the same
// registry answers differently an hour later, so the value has to be computed at
// scrape time. A measure registry does not: it is built once at the composition
// root and never mutated afterwards. Recomputing this on every scrape would
// re-answer a question whose inputs cannot change, so it is set once and the
// reason is written here rather than rediscovered by someone wondering why the
// two postures differ.

// MeasurePosture reports, for every measure in compute.Catalogue, whether this
// engine's registry serves it — and warns, once, listing the ones it does not.
//
// registry must be the SAME registry the query path uses. Passing a fresh one
// would report a posture no client experiences, which is the failure this
// function exists to end rather than reproduce.
func MeasurePosture(reg prometheus.Registerer, logger *slog.Logger, registry *compute.Registry) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_risk_measure_live",
		Help: "1 when this engine's registry serves the measure, 0 when the measure is implemented " +
			"but registered by nobody. A ZERO IS NOT AN ERROR ANYWHERE ELSE: the query path drops " +
			"unknown measure names, so a client asking for a dark measure receives 200 with it " +
			"absent — indistinguishable from a portfolio that holds none of that instrument (#509). " +
			"A ONE MEANS THE NAME IS REGISTERED, NOT THAT ITS FAMILY IS WIRED: Delta is served by " +
			"the RISK-07 net-exposure placeholder in DefaultRegistry, so it reads 1 under " +
			"family=greeks on an engine with no pricing-derived Greek at all. Read family " +
			"coverage from the family's OTHER members.",
	}, []string{"measure", "family"})
	reg.MustRegister(g)

	// WHICH ARE DARK IS ASKED OF compute.Dark, not recomputed here. The first
	// draft of this function rebuilt the served-set and did the subtraction
	// inline — a second implementation of the one concept, and
	// test/arch/no_dark_measure_seam_test.go failed it on the spot, because
	// compute.Dark then had no production caller. That is the guard working: a
	// copied helper is how a fix stops spreading.
	darkByFamily := map[compute.MeasureFamily][]string{}
	dark := map[v1.MeasureName]bool{}
	for _, m := range compute.Dark(registry) {
		dark[m.Name] = true
		darkByFamily[m.Family] = append(darkByFamily[m.Family], string(m.Name))
	}

	// EVERY CATALOGUED MEASURE GETS A SERIES, INCLUDING THE ZEROES. A gauge that
	// emitted only the live ones would climb from nothing to nothing and always
	// read as full coverage — and the dark ones are the entire question.
	live := 0
	for _, m := range compute.Catalogue() {
		if dark[m.Name] {
			g.WithLabelValues(string(m.Name), string(m.Family)).Set(0)
			continue
		}
		g.WithLabelValues(string(m.Name), string(m.Family)).Set(1)
		live++
	}

	total := len(compute.Catalogue())
	if len(darkByFamily) == 0 {
		logger.Info("risk measure posture: every implemented measure is registered",
			"measures", total)
		return
	}

	families := make([]string, 0, len(darkByFamily))
	for f, names := range darkByFamily {
		sort.Strings(names)
		families = append(families, string(f)+"["+strings.Join(names, ",")+"]")
	}
	sort.Strings(families)

	// WARN, not Info. "This engine cannot answer for two thirds of the measures
	// it implements" has to have been read before anyone builds a report on the
	// query API, and Info is where it gets filtered out.
	//
	// The message names the CONSEQUENCE rather than the count, because an
	// operator who reads "18 measures unregistered" reasonably concludes they are
	// optional extras rather than silently-absent answers.
	logger.Warn("risk-engine serves only part of the measure catalogue — a client that requests a "+
		"dark measure receives a SUCCESSFUL response with the measure simply absent, because the "+
		"query path drops unknown names. That is indistinguishable from a portfolio holding none "+
		"of that instrument, so nothing errors and no probe fails",
		"live", live, "dark", total-live, "catalogued", total,
		"dark_by_family", strings.Join(families, " "),
		"why", "each family is registered by one seam that no composition root calls; the seams and "+
			"the provider each is missing are listed in test/arch/no_dark_measure_seam_test.go (#509)",
		"gauge", "kanz_risk_measure_live")
}
