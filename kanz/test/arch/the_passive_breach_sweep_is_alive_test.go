package arch

import (
	"os"
	"strings"
	"testing"
)

// THE ONLY DETECTOR OF A PASSIVE BREACH MUST NOT BE ABLE TO STOP SILENTLY
// (#983).
//
// The compliance post-trade monitor re-runs every book it holds on an interval,
// and that loop is not an optimisation: a financed book that falls 20% raises
// its gross leverage with NO order placed anywhere, so nothing wakes the monitor
// and nothing else in this estate is watching for it. #787 built the cash and
// mark folds specifically so that breach becomes detectable after the trade.
//
// Before #983 the only signal that the loop had stopped was an ERROR log on a
// sweep that RAN AND FAILED. A stopped ticker, a goroutine that never started
// and a broker-less deployment all produced total silence — and the loop
// deliberately logs and continues rather than terminating, so a persistently
// failing sweep never surfaced as a crash either. Pods up, probes green, control
// dead.
//
// Three ways the repair rots, and the third has now happened three times:
//
//  1. A FAILED SWEEP STARTS COUNTING AS LIVENESS. If the loop advances the
//     last-success timestamp on the error path, a loop failing on its first book
//     for a week reports itself as current: the failure counter rises beside a
//     timestamp saying everything is fine, and the staleness rule never fires.
//
//  2. THE INTERVAL STOPS BEING EXPORTED. The alert is written in intervals —
//     "three times the deployment's own cadence" — precisely so that widening
//     COMPLIANCE_REEVALUATE_INTERVAL widens the bound with it. Hard-coding a
//     staleness is a rule that is correct only for the 60s default and stops
//     being correct without anybody editing it.
//
//  3. THE SERIES MOVE BEHIND THE BROKER BRANCH. A compliance with no
//     COMPLIANCE_NATS_URL starts no sweep at all, which is exactly what the
//     staleness rule pages on — and a rule over an absent series evaluates to
//     nothing. #973 (the copilot's answer metrics) and #963 (the cash-drag
//     series) were each this defect; both were caught only by running the built
//     binary, which no unit test reaches.

const (
	sweepMetricsRel = "../../services/compliance/cmd/compliance/sweepmetrics.go"
	sweepLoopRel    = "../../services/compliance/cmd/compliance/valuation.go"
	sweepMainRel    = "../../services/compliance/cmd/compliance/main.go"
	sweepRulesRel   = "../../infra/observability/alerts/operational.rules.yaml"
	sweepTestsRel   = "../../infra/observability/alerts/operational_test.yaml"
)

// TestOnlyACompletedSweepCountsAsLiveness holds rule one.
func TestOnlyACompletedSweepCountsAsLiveness(t *testing.T) {
	src := readStripped(t, sweepLoopRel)
	body := funcBody(t, src, "func reevaluateBooks(")

	fail := strings.Index(body, "mx.failed()")
	success := strings.Index(body, "mx.succeeded()")
	switch {
	case fail < 0:
		t.Fatal("the sweep loop no longer records a failed sweep. A sweep that runs and errors every " +
			"cycle would then be indistinguishable from one that is not running, and the two have " +
			"different owners — one bad book versus a dead loop (#983).")
	case success < 0:
		t.Fatal("the sweep loop no longer records a completed sweep, so " +
			"kanz_compliance_reevaluate_last_success_timestamp_seconds never advances and " +
			"CompliancePassiveBreachSweepStalled fires forever on a healthy estate (#983).")
	case success < fail:
		t.Fatal("the sweep loop records success BEFORE its failure branch, which means a failed sweep " +
			"advances the last-success timestamp. A loop failing on its first book for a week would " +
			"then report itself as current — the failure counter rising beside a timestamp saying " +
			"everything is fine — and the staleness rule, the one that says the control is not doing " +
			"its job, would never fire (#983).")
	}

	// A cancelled sweep is neither, or every clean shutdown raises an alert.
	if !strings.Contains(body, "ctx.Err() != nil") {
		t.Fatal("the sweep loop no longer distinguishes a CANCELLED sweep. On shutdown ReevaluateAll " +
			"returns ctx.Err(); counting that as a failure puts every clean shutdown into " +
			"CompliancePassiveBreachSweepFailing, and an alert that fires on normal operation gets " +
			"silenced (#983).")
	}
}

// TestTheSweepCadenceIsExported holds rule two.
func TestTheSweepCadenceIsExported(t *testing.T) {
	src := readStripped(t, sweepMetricsRel)
	if !strings.Contains(src, "kanz_compliance_reevaluate_interval_seconds") {
		t.Fatal("the configured sweep cadence is no longer exported. CompliancePassiveBreachSweepStalled " +
			"is written as 'three times the deployment's own interval' so that widening " +
			"COMPLIANCE_REEVALUATE_INTERVAL widens the bound with it; without the series the rule " +
			"has to hard-code a staleness, which is correct only for the 60s default and stops " +
			"being correct without anybody editing it (#983).")
	}

	rules, err := os.ReadFile(sweepRulesRel)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	if !strings.Contains(string(rules), "* kanz_compliance_reevaluate_interval_seconds") {
		t.Fatal("the staleness rule no longer multiplies by the exported interval, so it is written in " +
			"seconds rather than in intervals. A deployment that widens its sweep cadence would then " +
			"be paged on every cycle, and the rule would be silenced (#983).")
	}
}

// TestTheSweepSeriesAreRegisteredWithoutABroker holds rule three.
//
// STRUCTURAL, because the runtime symptom is an ABSENCE: a scrape with no
// kanz_compliance_reevaluate_ lines looks like a pod that has not started yet.
// The same shape shipped twice before and both were caught only by running the
// binary, which is why it is asserted here rather than left to review.
func TestTheSweepSeriesAreRegisteredWithoutABroker(t *testing.T) {
	src := readStripped(t, sweepMainRel)
	reg := strings.Index(src, "sweepMx.register(")
	consumers := strings.Index(src, "func runConsumers(")
	switch {
	case reg < 0:
		t.Fatal("compliance never registers the sweep-liveness series, so every rule over them queries " +
			"an empty vector and never fires (#983).")
	case consumers < 0:
		t.Fatal("runConsumers is gone — this guard's premise has moved and it must be rewritten against " +
			"whatever now decides whether the consumers start.")
	case reg > consumers:
		t.Fatal("sweepMx.register is called inside runConsumers, which only runs when " +
			"COMPLIANCE_NATS_URL is set. That deployment starts NO sweep at all — the exact state " +
			"CompliancePassiveBreachSweepStalled exists to page on — and it would export no series, " +
			"over which the rule evaluates to nothing. Register in run(), before the consumer " +
			"branch. This is the third time this estate has shipped that wiring: #973's answer " +
			"metrics and #963's cash-drag series were both this, and both were caught only by " +
			"running the built binary (#983).")
	}
}

// TestTheSweepAlertsHaveHarnessCasesIncludingNeverRun is the proof standard.
//
// THE never-run CASE IS THE ONE THAT MATTERS. The last-success gauge is
// deliberately NOT initialised to time.Now(), so a process whose loop never
// started reports 0 and the rule trips. A harness that only ever supplies a
// recently-advancing timestamp cannot tell that design from one that seeds the
// clock — and the seeded version would make a broker-less deployment look
// healthy for three intervals after every restart, forever.
func TestTheSweepAlertsHaveHarnessCasesIncludingNeverRun(t *testing.T) {
	b, err := os.ReadFile(sweepTestsRel)
	if err != nil {
		t.Fatalf("read alert tests: %v", err)
	}
	harness := string(b)

	for _, group := range []string{
		"the sweep has never run at all",
		"a deployment with a wider interval is judged against its own cadence",
		"one skipped sweep does not page",
	} {
		if !strings.Contains(harness, "name: "+group+"\n") {
			t.Fatalf("the alert harness no longer contains the %q case. Each of the three covers a "+
				"property no firing case can: that a never-started loop trips the rule, that a "+
				"deployment's own cadence is respected, and that ordinary jitter does not page.", group)
		}
	}
	for _, alert := range []string{"CompliancePassiveBreachSweepStalled", "CompliancePassiveBreachSweepFailing"} {
		if !strings.Contains(harness, "alertname: "+alert) {
			t.Fatalf("alert %s has no case in operational_test.yaml. A rule that has never been "+
				"executed is a claim, not a control.", alert)
		}
	}
}
