package main

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk-engine/internal/app"
)

// THE OUTPUT FACT PUBLISHER, AND THE GAUGE THAT SAYS WHICH MODEL IT ANNOUNCED
// (#1037).
//
// # Why they are built together
//
// risk.portfolio.measures_computed is the subject the OMS folds into the
// pre-trade gate, so it is the one place where "which model produced this
// number" decides whether an order is admitted. The observer therefore hangs off
// the publisher rather than off the registry: a registry probe would have to
// EXECUTE each measure to learn its model — fitting a live factor model and
// moving the factor skip counters at boot — and would still describe the registry
// rather than the numbers the gate actually folds.
//
// # Why the gauge is registered HERE, before every branch
//
// kanz_risk_measure_live answers whether a measure NAME is registered, and reads
// 1 for VaR99 whether it is historical simulation or compute.VaR99's
// 0.01 × GrossExposure. The placeholder is what every pod without
// RISK_ENGINE_MARKETDATA_DATABASE_URL serves, and no manifest in infra/ sets it —
// so the two configurations that matter most exported identical series, and "a
// mandate names a measure served by a placeholder" could not be alerted on.
//
// This runs ahead of the market-data branch and of everything that could take the
// other path, so the series exist in every deployment. A collector registered
// inside `if cfg.X != ""` exports nothing at all in the deployment taking the
// other branch, and an alert over it is then silent in exactly the state it was
// written for — which has shipped twice here (#973, #963).
//
// The returned posture still needs Seed(registry) once the registry is final; the
// composition root does that beside MeasurePosture, from the same registry the
// query path uses.
func newMeasurePublisher(producer *bus.Producer, reg prometheus.Registerer) (*publish.Publisher, *app.MeasureMethodPosture, error) {
	if producer == nil {
		return nil, nil, errors.New("risk-engine: measure publisher needs a producer")
	}
	methods := app.NewMeasureMethodPosture(reg)
	publisher, err := publish.NewPublisher(producer, publish.WithMeasureMethodObserver(methods.Observe))
	if err != nil {
		return nil, nil, err
	}
	return publisher, methods, nil
}
