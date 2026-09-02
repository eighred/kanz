package main

// Agent-plane metric tests (#973).
//
// These run against a REAL prometheus registry rather than asserting on the
// observer's arithmetic, because the failure mode being guarded is not a wrong
// number — it is a MISSING SERIES. An alert arm over a series no pod exports
// evaluates to nothing and never fires, so "the estate is watched" and "the
// estate is silent" look identical from a dashboard (#62 deleted ten rules in
// exactly that state). Only a registry can say whether a series exists at zero.

import (
	"sort"
	"strings"
	"testing"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/services/copilot/internal/agent"
)

// seriesIn returns the label-value → value map for one metric family.
func seriesIn(t *testing.T, g prometheus.Gatherer, name string) map[string]float64 {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			var parts []string
			for _, l := range m.GetLabel() {
				parts = append(parts, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(parts)
			out[strings.Join(parts, ",")] = metricValue(m)
		}
	}
	return out
}

func metricValue(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	return 0
}

// EVERY OUTCOME EXISTS AS AN EXPLICIT ZERO BEFORE THE FIRST QUESTION.
//
// This is the whole reason the labels are seeded. A counter with no series is
// not "zero" to PromQL — it is nothing, and increase() over nothing produces an
// empty vector that no threshold ever exceeds. So the state an operator most
// wants to trust ("the copilot has refused nothing today") is precisely the one
// an unseeded counter cannot express.
func TestEveryAnswerOutcomeIsSeededAtZero(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	got := seriesIn(t, reg, "kanz_copilot_answers_total")
	for _, want := range []string{"unspecified", "answered", "refused", "budget_exhausted", "failed"} {
		key := "outcome=" + want
		v, ok := got[key]
		if !ok {
			t.Fatalf("no series for outcome=%q at startup (have %v). An alert on this outcome would "+
				"query an empty vector and never fire, so the estate reads as watched and is not "+
				"(#973).", want, got)
		}
		if v != 0 {
			t.Fatalf("outcome=%q seeded at %v, want 0", want, v)
		}
	}
}

// THE LABEL SET COMES FROM THE SCHEMA, NOT FROM A LIST HERE.
//
// #806 (marks) and #803 (refusal flags) were both one defect: somebody
// enumerated a set by hand and missed a member. The fix in each was to derive
// the set from its source of truth. This asserts the derivation actually covers
// the enum rather than coincidentally matching today's five values.
func TestOutcomeLabelsCoverTheWholeWireEnum(t *testing.T) {
	vals := observationpb.AgentAnswerOutcome(0).Descriptor().Values()
	got := answerOutcomeLabels()
	if len(got) != vals.Len() {
		t.Fatalf("answerOutcomeLabels() returned %d labels for an enum with %d values (%v). A new "+
			"AgentAnswerOutcome would ship with no series behind it, and every alert on "+
			"kanz_copilot_answers_total would silently stop covering it.", len(got), vals.Len(), got)
	}
	for _, label := range got {
		if strings.HasPrefix(label, "AGENT_ANSWER_OUTCOME") || strings.ToLower(label) != label {
			t.Fatalf("label %q is not rendered as a metric label value", label)
		}
	}
}

// AN ANSWER MOVES THE SERIES IT SHOULD AND NOT THE ONES IT SHOULD NOT.
func TestACleanAnswerMovesOnlyTheOutcomeAndCoverageSeries(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	mx.observe(agent.Observation{
		Outcome:    observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		Grounded:   true,
		Turns:      2,
		ToolCalls:  3,
		Measured:   4,
		RecordKept: true,
	})

	if v := seriesIn(t, reg, "kanz_copilot_answers_total")["outcome=answered"]; v != 1 {
		t.Fatalf("answers_total{answered} = %v, want 1", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_tool_calls_total")["result=ok"]; v != 3 {
		t.Fatalf("tool_calls_total{ok} = %v, want 3 — all three calls succeeded", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_context_measures_total")["status=measured"]; v != 4 {
		t.Fatalf("context_measures_total{measured} = %v, want 4", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_ungrounded_answers_total")[""]; v != 0 {
		t.Fatalf("ungrounded_answers_total = %v on a grounded answer, want 0", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_unrecorded_answers_total")[""]; v != 0 {
		t.Fatalf("unrecorded_answers_total = %v on a recorded answer, want 0 — a healthy estate that "+
			"drives this series gets the critical rule silenced", v)
	}
}

// A REFUSAL IS NOT AN UNGROUNDED ANSWER.
//
// The gate marks a refusal grounded by construction, because it states no
// numbers. But a FAILED loop carries the zero value of Grounded — false — and
// counting that as an ungrounded answer would fill the series with transport
// faults and make CopilotUngroundedClaimsSustained fire on a broker outage.
func TestATransportFailureIsNotCountedAsUngrounded(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	mx.observe(agent.Observation{
		Outcome:    observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_FAILED,
		Grounded:   false, // the zero value, not a verdict
		RecordKept: true,
	})

	if v := seriesIn(t, reg, "kanz_copilot_ungrounded_answers_total")[""]; v != 0 {
		t.Fatalf("ungrounded_answers_total = %v after a FAILED loop, want 0. The grounding gate never "+
			"ran on this answer — Grounded is false because it is the zero value — so counting it "+
			"puts every upstream outage into a series operators read as 'the model invented a "+
			"number'.", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_answers_total")["outcome=failed"]; v != 1 {
		t.Fatalf("answers_total{failed} = %v, want 1 — the failure itself must still be counted", v)
	}
}

// AN UNGROUNDED ANSWER MOVES BOTH SERIES, at their own granularities: one answer
// and however many figures it invented. The ratio between them is what separates
// one inventive answer from a broad grounding failure.
func TestAnUngroundedAnswerCountsAnswersAndClaimsSeparately(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	mx.observe(agent.Observation{
		Outcome:          observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		Grounded:         false,
		UngroundedClaims: 3,
		RecordKept:       true,
	})

	if v := seriesIn(t, reg, "kanz_copilot_ungrounded_answers_total")[""]; v != 1 {
		t.Fatalf("ungrounded_answers_total = %v, want 1", v)
	}
	if v := seriesIn(t, reg, "kanz_copilot_ungrounded_claims_total")[""]; v != 3 {
		t.Fatalf("ungrounded_claims_total = %v, want 3 — the two series are the answer and the "+
			"figures, and collapsing them loses the ratio that names the failure", v)
	}
}

// TOOL ERRORS COME OUT OF THE OK COUNT, not in addition to it. ToolCalls is the
// total; treating it as the success count would put the error share permanently
// below its true value and delay CopilotToolErrorsRising.
func TestToolErrorsAreNotAlsoCountedAsSuccesses(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	mx.observe(agent.Observation{
		Outcome:    observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		Grounded:   true,
		ToolCalls:  5,
		ToolErrors: 2,
		RecordKept: true,
	})

	calls := seriesIn(t, reg, "kanz_copilot_tool_calls_total")
	if calls["result=ok"] != 3 || calls["result=error"] != 2 {
		t.Fatalf("tool_calls_total = %v, want ok=3 error=2 out of 5 calls. ToolCalls is the TOTAL, so "+
			"an ok count that ignores the errors understates the error share and delays the alert.",
			calls)
	}
}

// AN UNRECORDED ANSWER IS COUNTED WHATEVER THE OUTCOME. This is the series that
// distinguishes "nothing configured" from "checked, and fine".
func TestAnUnrecordedAnswerIsCounted(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	mx.observe(agent.Observation{
		Outcome:    observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		Grounded:   true,
		RecordKept: false,
	})

	if v := seriesIn(t, reg, "kanz_copilot_unrecorded_answers_total")[""]; v != 1 {
		t.Fatalf("unrecorded_answers_total = %v, want 1. An answer went out and nothing kept what the "+
			"model was shown; with this series flat, that state is indistinguishable from a "+
			"healthy one (#973).", v)
	}
}

// THE SEEDED SERIES EXIST BEFORE ANY ANSWER, for the whole family — not just the
// outcomes. A ratio rule divides by a counter an idle deployment never touches.
func TestTheLabelledFamiliesAreSeededAtZero(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	mx := newAnswerMetrics()
	mx.register(reg)

	for name, wantLabels := range map[string][]string{
		"kanz_copilot_tool_calls_total":       {"result=ok", "result=error"},
		"kanz_copilot_context_measures_total": {"status=measured", "status=withheld"},
	} {
		got := seriesIn(t, reg, name)
		for _, l := range wantLabels {
			if _, ok := got[l]; !ok {
				t.Fatalf("%s has no series for %s at startup (have %v). CopilotToolErrorsRising and "+
					"CopilotContextWithheld both divide by this family, and a missing denominator "+
					"makes the rule unfireable rather than quiet.", name, l, got)
			}
		}
	}
}
