package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

// AN EMPTY STREAM THAT READS AS "NOTHING HAS EVER FAILED" (#589).
//
// The post-trade plane in services/oms/internal/posttrade is written, tested and
// constructed by nobody. That much is ordinary — the dark-capability guard in
// test/arch tracks it, with an exemption naming this issue. What is NOT ordinary,
// and what this file exists for, is that the ESTATE AROUND IT IS ALREADY
// PROVISIONED:
//
//	infra/nats/bootstrap-job.yaml   ensure_stream SETTLEMENT "settlement.>" 168h
//	infra/nats/tenancy.yaml         grants oms publish on settlement.instruction.fail,
//	                                naming posttrade.BusFailSink by symbol
//
// So an operator asking "is settlement-fail monitoring live?" finds a stream, a
// broker grant and a named producer, and concludes yes. Then they query the
// stream and it is empty — and an empty SETTLEMENT stream is read the only way an
// empty stream of fails can be read: no settlement instruction has ever failed.
//
// That is a confident zero at the INFRASTRUCTURE layer, and it is this
// repository's own rule broken one level below where the rule is usually
// enforced: "nothing configured" and "checked, and fine" must never look the
// same. Today they are the same byte-for-byte — zero messages on
// settlement.instruction.fail.
//
// # What this does NOT do is start the plane
//
// Fail detection needs a counterparty confirmation feed — a broker, custodian or
// CSD confirmation — and nothing in this module receives one. SimSettlementVenue
// simulates the settlement-VENUE half only, and it settles on the settlement date
// by construction, so a plane run over it would emit a permanent stream of zero
// fails: a confident zero that is now ACTIVELY asserted rather than merely
// implied, which is worse. #345 already ruled on the class — a SUCCESSFUL run
// over invented inputs beats a failed one for exactly as long as it takes someone
// to trust it. So the posture is stated instead: not running, per stage, with
// what would arm it.
//
// # Why this lives in package main and does not import posttrade
//
// Deliberately. Importing posttrade from the composition root to announce that
// posttrade is dark would give the package an importer and silence the
// dark-capability guard that names it — reporting the absence by erasing the one
// signal that already tracks it. The stage table below therefore refers to
// posttrade's entry points BY NAME, and settlement_posture_test.go parses the
// package's source to prove the names are real and the table is complete.

// settlementPlaneMetric is the posture gauge's name. A gauge, not a counter of
// fails: the question it answers is "is this stage running", which has an answer
// even when nothing has happened.
const settlementPlaneMetric = "kanz_oms_settlement_plane_running"

// settlementStage is one stage of the POST-01/PARITY-04 post-trade plane: the
// entry points in services/oms/internal/posttrade that implement it, and the
// thing that would arm it.
type settlementStage struct {
	// stage is the metric label value. Stable — an alert is written against it.
	stage string
	// entrypoints are the exported top-level functions in posttrade that this
	// stage covers. Every one of them must be claimed by exactly one stage, and
	// settlement_posture_test.go parses the package to enforce that: a new
	// exported entry point arriving with no stage is the hand-enumerated-list
	// trap this table would otherwise carry (the seeding lists in
	// risk-engine/cmd/risk-engine/liquidity.go have it — a reason added to
	// liquiditysource and not to the list ships with no series at all).
	entrypoints []string
	// armedBy names what is missing, in operator terms. Not "TODO": the specific
	// input whose absence is the reason, so the next person can tell whether it
	// has arrived.
	armedBy string
}

// settlementStages is every stage of the post-trade plane this service ships the
// code for. Listed explicitly rather than derived from the package, because the
// mapping from a function to an operational stage is a judgement — but the
// COVERAGE of that list is derived, and tested.
var settlementStages = []settlementStage{
	{
		stage:       "confirmation_match",
		entrypoints: []string{"MatchFill", "Reconcile"},
		armedBy: "a counterparty confirmation feed — a broker, custodian or CSD delivering the " +
			"settlement.v1.TradeConfirmation half of a trade. Nothing in this module receives one. " +
			"It may not be simulated: an affirmation computed from invented counterparty terms " +
			"affirms a trade nobody confirmed (#345)",
	},
	{
		stage:       "instruction_generation",
		entrypoints: []string{"NewInstruction"},
		armedBy: "an affirmed match to generate from (confirmation_match), plus the custodian and " +
			"settlement-date reference data an instruction carries. The OMS's own fills are already " +
			"on the bus; the counterparty half is the missing side",
	},
	{
		stage:       "settlement_tracking",
		entrypoints: []string{"Instruct", "NewSimSettlementVenue", "NewCalendar"},
		armedBy: "a real posttrade.SettlementVenue. The only implementation is SimSettlementVenue, " +
			"which settles on the settlement date by construction and therefore never fails — " +
			"tracking against it would publish a permanent, ACTIVE claim of zero fails in place of " +
			"today's empty one",
	},
	{
		stage:       "fail_detection",
		entrypoints: []string{"DetectFails", "DetectFailsWithCalendar"},
		armedBy: "settlement state to scan. DetectFails ages INSTRUCTED and FAILED instructions, " +
			"and nothing creates either, so it would run over an empty slice every tick and report " +
			"no fails — the exact sentence this posture exists to stop the estate from implying",
	},
	{
		stage:       "fail_publication",
		entrypoints: []string{"EmitFails", "NewBusFailSink", "NewBusFailSinkWithClock", "EncodeFail"},
		armedBy: "detected fails to publish. NOTHING ELSE: this entry used to also name \"a " +
			"posttrade.FailEncoder over the settlement.v1 SDK (generated-not-committed, EVT-15a)\", " +
			"and that half was never true — kanz-schemas/gen is not checked in AT ALL, so every " +
			"generated package is generated-not-committed and settlement.v1 was always importable. " +
			"posttrade.EncodeFail is now that mapping, concrete and tested, and the encoder seam it " +
			"replaced is gone. THIS IS THE STAGE THE ESTATE ALREADY CLAIMS: the SETTLEMENT stream " +
			"is provisioned and the oms workload holds the publish grant on " +
			"settlement.instruction.fail, so the one stage with infrastructure behind it is still " +
			"the one with no process behind it",
	},
}

// settlementPlanePosture records, for every stage of the post-trade plane this
// service implements, whether it is running — and states what would arm the ones
// that are not, once at startup and continuously as a gauge.
//
// running maps a stage name to whether the composition root wired it. It is nil
// today, and the parameter is the seam rather than a hardcoded zero on purpose:
// the day a stage IS wired, the honest report is one line at its call site, not
// an edit to a table of constants that someone will forget.
//
// THE GAUGE IS THE DURABLE HALF. A startup WARN is gone by the next rollout;
// kanz_oms_settlement_plane_running is queryable at 3am when an operations desk
// asks why the SETTLEMENT stream is empty, and it distinguishes the two answers
// that look identical from the broker:
//
//	stage="fail_detection" == 0   fail detection is not running on this deployment
//	stage="fail_detection" == 1   it is running, and nothing has failed
//
// EVERY STAGE GETS A SERIES, INCLUDING THE ZEROES — the whole point. A gauge that
// appeared only for running stages would answer an alert with "no data", which is
// the empty-stream ambiguity moved one system to the left.
func settlementPlanePosture(reg prometheus.Registerer, logger *slog.Logger, running map[string]bool) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: settlementPlaneMetric,
		Help: "1 when this stage of the OMS post-trade plane is running, 0 when the code for it " +
			"ships and no process constructs it. A zero is a posture, not an error — but the " +
			"SETTLEMENT stream is provisioned either way, and an empty stream of settlement fails " +
			"reads as 'nothing has ever failed' (#589).",
	}, []string{"stage"})
	reg.MustRegister(g)

	var on, off []string
	var armedBy []string
	for _, s := range settlementStages {
		if running[s.stage] {
			g.WithLabelValues(s.stage).Set(1)
			on = append(on, s.stage)
			continue
		}
		g.WithLabelValues(s.stage).Set(0)
		off = append(off, s.stage)
		armedBy = append(armedBy, s.stage+": "+s.armedBy)
	}
	sort.Strings(on)
	sort.Strings(off)
	sort.Strings(armedBy)

	if len(off) == 0 {
		logger.Info("oms settlement plane: every implemented post-trade stage is running",
			"stages", on, "metric", settlementPlaneMetric)
		return
	}

	// WARN, not Info, and the same reasoning as the calibration posture it copies:
	// Info is where "the settlement-fail stream on this deployment has no
	// producer" gets filtered out, and that is the sentence an operator must have
	// read BEFORE they conclude anything from the stream being empty.
	logger.Warn("oms settlement plane: SETTLEMENT-FAIL DETECTION IS NOT RUNNING ON THIS DEPLOYMENT — "+
		"the SETTLEMENT stream is provisioned and this workload holds the publish grant on "+
		"settlement.instruction.fail, and NO PROCESS CONSTRUCTS A PRODUCER FOR IT. The stream is "+
		"therefore permanently empty, which is indistinguishable from 'no settlement instruction has "+
		"ever failed'. Do not read that stream as a clean book (#589)",
		"stream", "SETTLEMENT",
		"subject", "settlement.instruction.fail",
		"producer", "services/oms/internal/posttrade.BusFailSink (defined, never constructed)",
		"running", on,
		"not_running", off,
		"armed_by", armedBy,
		"metric", settlementPlaneMetric)
}
