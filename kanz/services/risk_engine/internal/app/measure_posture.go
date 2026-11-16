package app

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

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
			"A ONE MEANS THE NAME IS REGISTERED, AND NOTHING ABOUT WHAT SERVES IT: Delta reads 1 " +
			"under family=greeks whether it is a pricing-derived Greek or the RISK-07 net-exposure " +
			"placeholder, and VaR99 reads 1 whether it is historical simulation or 1%×gross. " +
			"WHICH ONE IS kanz_risk_measure_method's question, and it is the one that decides " +
			"whether the number may gate an order (#1037). Read family coverage from the family's " +
			"OTHER members.",
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

// WHICH MODEL IS THIS ENGINE ACTUALLY ANNOUNCING (#1037)?
//
// # The question kanz_risk_measure_live cannot answer
//
// That gauge asks whether the NAME is registered, and reads 1 in both
// configurations of the fork that matters: VaR99 served by historical simulation
// over a price panel, and VaR99 served by compute.VaR99 — 0.01 × GrossExposure, an
// illustrative constant. Which one a pod serves turns on
// RISK_ENGINE_MARKETDATA_DATABASE_URL, which no manifest in infra/ sets, so the
// shipped rollout has been answering with 1% of gross while exporting the series
// of an engine that computes a real number.
//
// The alert an operator needs — "a mandate names a measure served by a
// placeholder" — was unwritable, because both states looked the same. With the
// method as a label it is one expression:
//
//	kanz_risk_measure_method{method="placeholder_1pct_gross"} == 1
//
// # Why it is fed from the announcement rather than probed from the registry
//
// A registry probe has to EXECUTE a measure to learn its model, and executing
// them at boot fits a live factor model and moves the factor skip counters for a
// portfolio nobody owns. It would also answer about the REGISTRY, while the thing
// that gates order admission is the measures FACT the OMS folds. So the publisher
// reports what it announced and this records it — measured behaviour, not the
// registry's intent.
//
// # Why registration and setting are separated
//
// The collector is registered by NewMeasureMethodPosture, which the composition
// root calls UNCONDITIONALLY — before the market-data branch, before the
// publisher, before anything that could take the other path. A collector created
// inside `if producer != nil` exports no series at all in the deployment that
// takes the other branch, and an == 0 (or absent-series) alert over it is then
// silent in exactly the state it was written for. That has shipped twice here
// (#973, #963).
type MeasureMethodPosture struct {
	g *prometheus.GaugeVec

	mu   sync.Mutex
	last map[string]string
}

// MethodUnobserved labels a measure this engine has not yet announced. It is a
// REAL STATE AND NOT A GAP: a served measure whose series still reads
// method="unobserved" long after boot means the engine has computed nothing for
// it, which an absent series would have left invisible.
const MethodUnobserved = "unobserved"

// MethodUndeclared labels a measure whose producer named no model. Distinct from
// unobserved: something WAS announced and it said nothing about how it was
// computed. Every measure this platform published before #1037 is in that state,
// so it must not be read as a verdict either way.
const MethodUndeclared = "undeclared"

// NewMeasureMethodPosture registers the gauge. Call it unconditionally, at the
// composition root, ahead of every branch.
func NewMeasureMethodPosture(reg prometheus.Registerer) *MeasureMethodPosture {
	p := &MeasureMethodPosture{
		g: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_risk_measure_method",
			Help: "1 for the model this engine last announced for the measure, 0 for a model it has " +
				"stopped announcing. THE LABEL IS THE ANSWER: method=\"placeholder\"_1pct_gross or " +
				"method=\"net_exposure_placeholder\" means the number is an ILLUSTRATIVE CONSTANT rather " +
				"than a calibrated risk figure, and the OMS admission gate refuses to fold it — a " +
				"mandate naming that measure REFUSES orders. method=\"undeclared\" means the producer " +
				"named no model, which is not a claim that the number is real. method=\"unobserved\" " +
				"means the measure is registered and nothing has been computed for it yet. " +
				"kanz_risk_measure_live answers whether the NAME is registered; this answers what " +
				"serves it, which is what decides whether the number may gate an order (#1037).",
		}, []string{"measure", "method"}),
		last: map[string]string{},
	}
	reg.MustRegister(p.g)
	return p
}

// Seed gives every measure the registry serves a series before the first
// recompute, so a dashboard is not empty and an alert is not silent while the
// engine warms up. Called with the SAME registry the query path uses, beside
// MeasurePosture and for the same reason.
func (p *MeasureMethodPosture) Seed(registry *compute.Registry) {
	if p == nil || registry == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range registry.Names() {
		p.g.WithLabelValues(string(name), MethodUnobserved).Set(1)
		p.last[string(name)] = MethodUnobserved
	}
}

// Observe records the model announced for one measure. It is
// publish.WithMeasureMethodObserver's callback, so it runs once per measure per
// measures FACT — cheap, and on the path whose output the OMS gate folds.
func (p *MeasureMethodPosture) Observe(measure, method string) {
	if p == nil || measure == "" {
		return
	}
	if method == "" {
		method = MethodUndeclared
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.last[measure]; ok {
		if prev == method {
			return
		}
		// THE PREVIOUS SERIES IS ZEROED, NOT DELETED. A deleted series makes an
		// alert stop evaluating rather than evaluate to false, which is the same
		// silence this gauge exists to end.
		p.g.WithLabelValues(measure, prev).Set(0)
	}
	p.g.WithLabelValues(measure, method).Set(1)
	p.last[measure] = method
}
