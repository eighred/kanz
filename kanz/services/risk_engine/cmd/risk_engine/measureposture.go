package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
)

// WHAT THIS ENGINE SERVES, WHAT SERVES IT, AND WHETHER ANY OF IT IS A MODEL.
//
// # Why this is a file rather than thirty lines in runEngine
//
// Three postures now answer three different questions about the SAME final
// registry, and each one exists because the previous one could not answer the
// next question:
//
//	kanz_risk_measure_live     is the NAME registered?              (#509)
//	kanz_risk_measure_method   what did it last ANNOUNCE?           (#1037)
//	kanz_risk_greek_model_live is the Greek family model-served?    (#1055)
//
// composition_root_length_test.go ratchets runEngine, and it refused the third
// one — the same way it refused #713's shutdown wiring and #1007's registry
// binding on the OMS side. So the wiring moved here instead of the budget going
// up, and services/oms/cmd/oms/riskfold.go is the shape being copied.
//
// # They must be given the SAME registry the query path uses
//
// A posture computed from a fresh registry describes an engine nobody talks to.
// That is MeasurePosture's own rule and it now binds three callers, which is
// precisely why they are one call: three call sites are three chances to pass a
// different registry to one of them, and the disagreement would be invisible —
// each gauge would be internally consistent and they would describe different
// engines.
//
// # Why they are stated the instant the registry is final
//
// For the reason CalibrationPosture states about itself: the case that matters
// most is the one where startup does not get that far. A pod that dies wiring the
// bus still has a complete answer to "what would this build have served", and
// that answer is a static fact about the binary rather than about the run.
//
// THE COUNT IS NOT A FIXED PROPERTY OF THE BINARY — it is what the config happens
// to unlock, which is exactly why the gauges exist rather than a number in a
// comment. That number used to say "EIGHT of twenty-six" and was stale within the
// day: FACTOR-01c, FI-01d and LIQ-01d each added to it. A fully configured pod
// today serves sixteen of the twenty-six catalogued measures; one with no
// market-data DSN serves five. The query path DROPS the rest —
// engine.filterMeasures discards unknown names — so they are absent from a 200
// rather than refused, which is indistinguishable from a portfolio that holds
// none of that instrument.
//
// Registration belongs to startup, before the broker branch. This operation
// only states the same final registry consumed by queries and recomputes.
func stateMeasurePostures(
	logger *slog.Logger,
	registry *compute.Registry,
	postures *startupPostures,
	greeks *app.GreekModelPosture,
) {
	postures.measures.State(logger, registry)
	postures.methods.Seed(registry)
	// AND WHETHER ANY GREEK IS A GREEK (#1055). The gauge was registered before
	// the broker branch and already reads 0 for the whole family; this is where the
	// registry gets to raise it, and where the pod says out loud that a mandate
	// naming Delta refuses every order until RegisterGreeks has a caller.
	greeks.State(logger, registry)
}

type startupPostures struct {
	bus      *bus.BusMetrics
	measures *app.MeasureRegistryPosture
	methods  *app.MeasureMethodPosture
}

// newStartupPostures owns one registry's broker and measure collectors before
// startup can take either arm. source must be the same NATSConfig.Name used by
// the connection callbacks; configuration is not a connectivity observation.
func newStartupPostures(reg prometheus.Registerer, source string, brokerConfigured bool) *startupPostures {
	postures := &startupPostures{
		bus:      bus.NewBusMetrics(reg),
		measures: app.NewMeasureRegistryPosture(reg),
		methods:  app.NewMeasureMethodPosture(reg),
	}
	postures.bus.ConfigureClient(source, brokerConfigured)
	postures.methods.Unconfigured()
	return postures
}
