package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

const settlementPlaneMetric = "kanz_oms_settlement_plane_running"
const settlementApplicabilityMetric = "kanz_oms_settlement_plane_applicability"

type settlementModel string

const (
	settlementUnknown    settlementModel = "unknown"
	settlementDirectSpot settlementModel = "direct_spot"
	settlementDeferred   settlementModel = "deferred"

	// A build capability, not deployment health or a config switch blessing an
	// unknown venue. The architecture guard binds this to the known direct-spot
	// adapters and accounting's settlement convention.
	wiredSettlementModel = settlementDirectSpot
)

type settlementStage struct {
	stage       string
	entrypoints []string
	armedBy     string
}

// These are the separate confirmation/instruction lifecycle's entry points.
// Direct spot settles through venue evidence and the accounting book; it must
// not acquire a second ledger or redundant instructions to make these gauges 1.
// Names avoid importing unused code merely to silence the dark-package guard.
var settlementStages = []settlementStage{
	{
		stage: "confirmation_match", entrypoints: []string{"MatchFill", "Reconcile"},
		armedBy: "a supported settlement convention requiring independent trade confirmation and an authenticated source for that evidence",
	},
	{
		stage: "instruction_generation", entrypoints: []string{"NewInstruction"},
		armedBy: "an independently affirmed trade requiring a separate settlement instruction, with trusted account, currency and instrument conventions",
	},
	{
		stage: "settlement_tracking", entrypoints: []string{"Instruct", "NewSimSettlementVenue", "NewCalendar"},
		armedBy: "an authoritative settlement acknowledgement/status interface and market calendar; SimSettlementVenue is not settlement evidence",
	},
	{
		stage: "fail_detection", entrypoints: []string{"DetectFails", "DetectFailsWithCalendar"},
		armedBy: "durable outstanding instructions under an applicable deferred settlement convention",
	},
	{
		stage: "fail_publication", entrypoints: []string{"EmitFails", "NewBusFailSink", "NewBusFailSinkWithClock", "EncodeFail"},
		armedBy: "detected instruction fails committed with a durable outbox; the provisioned SETTLEMENT stream alone is not a producer",
	},
}

// Neither metric certifies an individual trade, a live exchange connection or
// launch readiness. Unknown applicability is never silently treated as spot.
func settlementPlanePosture(reg prometheus.Registerer, logger *slog.Logger, running map[string]bool, model settlementModel) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: settlementPlaneMetric,
		Help: "1 when this deferred post-trade stage is wired, 0 otherwise. Consult kanz_oms_settlement_plane_applicability; this is not direct-spot settlement or launch readiness.",
	}, []string{"stage"})
	app := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: settlementApplicabilityMetric,
		Help: "One-hot build applicability of the separate confirmation/instruction plane: not_required for supported direct spot, required for deferred settlement, unknown otherwise. Not deployment health.",
	}, []string{"status"})
	reg.MustRegister(g, app)
	applicability := "unknown"
	switch model {
	case settlementDirectSpot:
		applicability = "not_required"
	case settlementDeferred:
		applicability = "required"
	default:
		model = settlementUnknown
	}
	for _, status := range []string{"not_required", "required", "unknown"} {
		value := 0.0
		if status == applicability {
			value = 1
		}
		app.WithLabelValues(status).Set(value)
	}
	var on, off, armedBy []string
	for _, s := range settlementStages {
		if running[s.stage] {
			g.WithLabelValues(s.stage).Set(1)
			on = append(on, s.stage)
		} else {
			g.WithLabelValues(s.stage).Set(0)
			off = append(off, s.stage)
			armedBy = append(armedBy, s.stage+": "+s.armedBy)
		}
	}
	sort.Strings(on)
	sort.Strings(off)
	sort.Strings(armedBy)
	if model == settlementDirectSpot && len(on) == 0 {
		logger.Info("oms settlement model: direct-venue spot; separate confirmation and settlement instructions are not required for the supported adapters",
			"model", model, "applicability", applicability, "not_running", off,
			"evidence_path", "venue execution history -> OMS recovery -> accounting settled ledger -> exchange balance reconciliation",
			"scope", "build capability only; live evidence and P0 launch gates remain mandatory",
			"stream", "SETTLEMENT", "metric", settlementPlaneMetric)
		return
	}
	if model == settlementDeferred && len(off) == 0 {
		logger.Info("oms settlement plane: every implemented post-trade stage is wired; individual settlement still requires authoritative evidence",
			"model", model, "applicability", applicability, "stages", on, "metric", settlementPlaneMetric)
		return
	}
	message := "oms required deferred settlement stages NOT RUNNING ON THIS DEPLOYMENT; an empty SETTLEMENT stream is not a clean book"
	if model == settlementUnknown {
		message = "oms settlement applicability UNKNOWN; stage wiring alone cannot establish settlement coverage"
	} else if model == settlementDirectSpot {
		message = "oms settlement posture inconsistent: deferred stages wired for a direct-spot build; review applicability"
	}
	logger.Warn(message,
		"model", model, "applicability", applicability,
		"stream", "SETTLEMENT", "subject", "settlement.instruction.fail",
		"producer", "posttrade.BusFailSink (separate instruction lifecycle)",
		"running", on, "not_running", off, "armed_by", armedBy, "metric", settlementPlaneMetric)
}
