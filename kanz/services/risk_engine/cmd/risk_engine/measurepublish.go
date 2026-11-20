package main

import (
	"errors"

	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
)

// THE OUTPUT FACT PUBLISHER, AND THE GAUGE THAT SAYS WHICH MODEL IT ANNOUNCED
// (#1037).
//
// # Why the startup observer is attached to the publisher
//
// risk.portfolio.measures_computed is the subject the OMS folds into the
// pre-trade gate, so it is the one place where "which model produced this
// number" decides whether an order is admitted. The observer therefore hangs off
// the publisher rather than off the registry: a registry probe would have to
// EXECUTE each measure to learn its model — fitting a live factor model and
// moving the factor skip counters at boot — and would still describe the registry
// rather than the numbers the gate actually folds.
//
// # Why registration belongs to startup, before every branch
//
// kanz_risk_measure_live answers whether a measure NAME is registered, and reads
// 1 for VaR99 whether it is historical simulation or compute.VaR99's
// 0.01 × GrossExposure. The placeholder is what every pod without
// RISK_ENGINE_MARKETDATA_DATABASE_URL serves, and no manifest in infra/ sets it —
// so the two configurations that matter most exported identical series, and "a
// mandate names a measure served by a placeholder" could not be alerted on.
//
// Startup registers the observer before the broker branch. Accept that same
// instance here so published methods update the series scraped before any
// connection or computation existed.
func newMeasurePublisher(producer *bus.Producer, methods *app.MeasureMethodPosture) (*publish.Publisher, error) {
	if producer == nil {
		return nil, errors.New("risk-engine: measure publisher needs a producer")
	}
	if methods == nil {
		return nil, errors.New("risk-engine: measure publisher needs startup posture")
	}
	publisher, err := publish.NewPublisher(producer, publish.WithMeasureMethodObserver(methods.Observe))
	if err != nil {
		return nil, err
	}
	return publisher, nil
}
