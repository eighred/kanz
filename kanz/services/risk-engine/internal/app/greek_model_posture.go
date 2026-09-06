package app

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
)

// NO DEPLOYMENT OF THIS PLATFORM COMPUTES A GREEK, AND Delta ANSWERS ANYWAY
// (#1055).
//
// # The state this states
//
// compute.Delta returns sumInBaseCurrency(p, false) — NetExposure under a second
// name, the "every $1 of position is $1 of delta to spot" assumption. It is in
// compute.DefaultRegistry, which every engine builds, and compute.RegisterGreeks
// is the only thing that overwrites it. RegisterGreeks has NO PRODUCTION CALLER
// anywhere in this estate: it needs a VolProvider, which needs option premiums
// nothing here persists (#203/#345/#113). So the placeholder is not a
// misconfiguration one deployment fell into — unlike the VaR placeholder, which
// a market-data DSN switches off, there is no environment in which this one does
// not serve.
//
// Since #1037 the placeholder declares itself and the OMS admission gate refuses
// to fold it, so a mandate naming Delta is a PERMANENT REFUSAL: the measure is
// UNKNOWN, and compliance.RiskLimitRule fails closed on unknown. That direction
// is right — a control that refuses beats one that passes on a number nobody can
// vouch for — and it is a live change in admission behaviour that no signal
// announced. This is the signal.
//
// # Why Delta stays registered, which is the decision this file records (#1055)
//
// The alternative was to drop Delta from DefaultRegistry so the dark Greek family
// reads honestly as 0-of-5. It was rejected, and the reason is the one
// services/oms/cmd/oms/riskfold.go states about its own three counters: the
// refusals must stay distinguishable. Unregistering Delta does not remove the
// risk, it removes the EVIDENCE — the mandate would still refuse, but through the
// never-announced arm, which is the same arm a risk-engine outage takes. An
// operator would then read one signal for two incidents with different owners and
// different fixes. Worse, both existing signals go quiet with it
// (kanz_risk_measure_method stops reporting a placeholder, and the OMS's
// kanz_oms_risk_measures_placeholder_total stops counting), so the estate's
// dashboards would improve while nothing about its ability to measure delta
// changed. Deleting the subject of a statement is not a way of making the
// statement.
//
// Two more things weigh the same way. #1037 chose refuse-not-annotate for the VaR
// placeholder rather than deleting it, and #572 chose attach-coverage for
// structMeasure rather than deleting it; delete-not-refuse here would be a second
// answer to a question this repository has already settled twice. And the number
// is not an invented constant the way 1%×gross is: greeks.go prices a position
// with no contract terms linearly at Δ≡1 and sums its signed MarketValue, which is
// exactly what compute.Delta returns — so on this estate's spot-only book the
// placeholder and the pricing-derived Greek agree. It is a placeholder because
// the engine cannot PROVE the book holds no derivative, not because the arithmetic
// is fiction.
//
// # Why a gauge on top of the two that already exist
//
// kanz_risk_measure_live answers whether the NAME is registered, and reads 1 for
// Delta under family=greeks on an engine that computes no Greek at all —
// catalogue.go records that as a known falsehood it declined to fix in the
// catalogue. kanz_risk_measure_method answers what was last ANNOUNCED, which is
// the right question and only has an answer after a recompute: on a pod holding no
// portfolio it reads method="unobserved" forever, so an alert selecting the
// placeholder method is silent on exactly the engine that has never computed
// anything. This one is a property of the BUILD's registry, known at startup,
// before any portfolio exists.
//
// # The claim is derived, not typed in
//
// This file contains no list of Greek names and no sentence asserting that
// RegisterGreeks is unwired. The family comes from compute.Catalogue() and what is
// served comes from compute.Dark — the same two sources MeasurePosture reads — and
// the inference is structural: RegisterGreeks installs all five Greeks in one
// call, so if ANY member of the family is dark then RegisterGreeks did not run,
// and every member that does answer is answering from something else. The day it
// is wired, the higher-order Greeks light up and this posture flips itself.
// test/arch/greek_posture_is_stated_test.go pins the premise that inference rests
// on, so it fails if a second registrar for a Greek ever appears rather than
// quietly becoming wrong.

// GreekModelPosture reports, per catalogued Greek, whether this engine computes
// it from an option-pricing model.
type GreekModelPosture struct {
	g *prometheus.GaugeVec
}

// NewGreekModelPosture registers kanz_risk_greek_model_live and seeds every
// catalogued Greek at 0.
//
// CALL IT UNCONDITIONALLY, AT THE COMPOSITION ROOT, AHEAD OF THE BROKER BRANCH.
// A collector created inside `if cfg.NATSURL != ""` exports NO series on the
// deployment that takes the other path, and an `== 0` alert over it is then
// silent in exactly the state it was written for. That has shipped twice here
// (#973, #963) and #1050 fixed it once already in this very file's composition
// root.
//
// SEEDED AT ZERO RATHER THAN LEFT TO State. A pod that dies wiring the spine
// never reaches State, and "this build serves no Greek model" is a true statement
// about it. Seeding at 1 would be the opposite claim made by default.
func NewGreekModelPosture(reg prometheus.Registerer) *GreekModelPosture {
	p := &GreekModelPosture{
		g: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_risk_greek_model_live",
			Help: "1 when this engine computes the Greek from an option-pricing model " +
				"(compute.RegisterGreeks is wired), 0 when it does not. A ZERO ON Delta IS NOT " +
				"THE SAME KIND OF ZERO AS ONE ON Gamma: Delta is registered in " +
				"compute.DefaultRegistry and ANSWERS — with the portfolio's signed net exposure, " +
				"declared net_exposure_placeholder — which the OMS admission gate refuses to " +
				"fold, so a mandate naming Delta REFUSES every order rather than checking one " +
				"(#1037, #1055). Gamma/Vega/Theta/Rho are unregistered and simply absent from " +
				"every response. kanz_risk_measure_live answers whether the NAME is registered " +
				"and reads 1 for Delta in both states; kanz_risk_measure_method answers what was " +
				"last ANNOUNCED and has no answer at all until a recompute has run. This one is " +
				"a property of the registry, known at startup.",
		}, []string{"measure"}),
	}
	reg.MustRegister(p.g)
	for _, name := range cataloguedGreekNames() {
		p.g.WithLabelValues(string(name)).Set(0)
	}
	return p
}

// State sets the gauge from the registry this engine actually serves and says out
// loud what it found. It returns the Greek names that ARE served and are not
// model-derived — today exactly one, Delta — sorted, so a caller can assert on it.
//
// registry must be the SAME registry the query path uses, for MeasurePosture's
// reason: a posture computed from a fresh registry describes an engine nobody
// talks to.
func (p *GreekModelPosture) State(logger *slog.Logger, registry *compute.Registry) []string {
	if p == nil || registry == nil {
		return nil
	}
	family := cataloguedGreekNames()

	// WHICH ARE DARK IS ASKED OF compute.Dark, not recomputed here — the same
	// source MeasurePosture reads, so the two postures cannot disagree about which
	// names this engine serves.
	dark := map[v1.MeasureName]bool{}
	for _, m := range compute.Dark(registry) {
		dark[m.Name] = true
	}

	// THE INFERENCE, AND IT IS THE WHOLE FILE. RegisterGreeks installs every
	// member of this family in one call, so a single dark member proves it did not
	// run — and therefore that any member which DOES answer is answering from
	// something that is not an option-pricing model. Nothing here asserts that
	// RegisterGreeks is unwired; it is read off the registry, and it stops being
	// true by itself on the day somebody wires it.
	modelServed := true
	for _, name := range family {
		if dark[name] {
			modelServed = false
			break
		}
	}

	var placeholderServed []string
	for _, name := range family {
		if modelServed {
			p.g.WithLabelValues(string(name)).Set(1)
			continue
		}
		p.g.WithLabelValues(string(name)).Set(0)
		if !dark[name] {
			placeholderServed = append(placeholderServed, string(name))
		}
	}
	sort.Strings(placeholderServed)

	if modelServed {
		logger.Info("risk-engine: every Greek is computed from an option-pricing model",
			"measures", greekNameStrings(family), "gauge", "kanz_risk_greek_model_live")
		return nil
	}

	// WARN, not Info. "A mandate naming Delta refuses every order this pod is asked
	// about" has to have been read before somebody writes one, and Info is where it
	// gets filtered out.
	//
	// The message names the CONSEQUENCE rather than the count: an operator who
	// reads "4 Greeks unregistered" concludes they are optional extras, when the
	// fact that matters is that the fifth one answers and its answer gates nothing.
	logger.Warn("risk-engine: NO GREEK IS COMPUTED FROM AN OPTION-PRICING MODEL, and the Greek "+
		"names this engine does serve answer with the portfolio's net exposure — a number the "+
		"OMS admission gate refuses to fold, so a mandate naming one of them REFUSES every "+
		"order rather than checking it",
		"served_by_placeholder", placeholderServed,
		"unregistered", greekNameStrings(darkOf(family, dark)),
		"why", "compute.RegisterGreeks has no production caller: it needs a VolProvider, which "+
			"needs option premiums nothing in this estate persists",
		"would_arm_it", "a calibrated volatility surface behind compute.VolProvider plus option "+
			"contract terms (#203, #345, #113); wiring it against an uncalibrated surface would "+
			"return a MEASURED ZERO instead of a refusal, which is worse (#1044)",
		"gauge", "kanz_risk_greek_model_live")
	return placeholderServed
}

// cataloguedGreekNames returns the Greek family from compute.Catalogue in stable
// order. DERIVED, so a Greek added to the catalogue is covered without anyone
// remembering this file — the failure mode #588's posture map is shaped to avoid.
func cataloguedGreekNames() []v1.MeasureName {
	var out []v1.MeasureName
	for _, m := range compute.Catalogue() {
		if m.Family == compute.FamilyGreeks {
			out = append(out, m.Name)
		}
	}
	return out
}

// darkOf returns the members of family that the registry does not serve.
func darkOf(family []v1.MeasureName, dark map[v1.MeasureName]bool) []v1.MeasureName {
	var out []v1.MeasureName
	for _, name := range family {
		if dark[name] {
			out = append(out, name)
		}
	}
	return out
}

// greekNameStrings renders measure names for a log attribute.
func greekNameStrings(names []v1.MeasureName) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}
