package arch

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE AGENT PLANE MUST STAY MEASURABLE (#973).
//
// #971 made the agent's behaviour recordable — outcome, grounding verdict, tool
// errors, and how much of the book the read plane would actually state. #973 made
// it COUNTABLE, which is a different obligation: the record explains one answer to
// somebody who already has a complaint, and the metrics are how the estate notices
// there is something to complain about.
//
// Four ways that erodes, each with a precedent here:
//
//  1. AN ALERT OUTLIVES THE SERIES IT READS, or a series is registered with no
//     alert reading it. #62 deleted ten data-quality rules because each queried a
//     series no running pod exported, and a rule over an empty vector never fires
//     — the estate reads as alerted and is not. Both directions are checked.
//
//  2. THE OUTCOME LABELS GET WRITTEN OUT BY HAND. answerOutcomeLabels reads the
//     proto descriptor precisely so a new AgentAnswerOutcome cannot ship with no
//     series behind it. #806 (marks) and #803 (refusal flags) are both defects
//     where somebody enumerated a set by hand and missed a member, and the fix in
//     each was to derive the set from its source of truth rather than to
//     re-enumerate it more carefully.
//
//  3. THE METRICS GET WIRED BEHIND THE RECORDER. They were, in the first cut of
//     this work: registration sat inside the `if answerProducer != nil` branch, so
//     the deployment that kept NO record of anything also exported no series to say
//     so. That is the "nothing configured looks identical to checked, and fine"
//     failure in its purest form, and it is the exact state
//     kanz_copilot_unrecorded_answers_total exists to make visible.
//
//  4. A COVERAGE COUNT STARTS BEING DERIVED FROM A NIL POINTER instead of from
//     the measure's Status. measureread already draws that line once; a second
//     derivation is how a withheld measure starts being counted as present, which
//     is #757 wearing a different hat.

const (
	copilotMetricsRel = "../../services/copilot/cmd/copilot/answermetrics.go"
	copilotMainRel    = "../../services/copilot/cmd/copilot/main.go"
	copilotAgentDir   = "../../services/copilot/internal/agent"
	copilotGovernPath = "../../services/copilot/internal/governed/governed.go"
	copilotRulesRel   = "../../infra/observability/alerts/operational.rules.yaml"
	copilotTestsRel   = "../../infra/observability/alerts/operational_test.yaml"
)

// copilotDiagnosticOnlySeries names agent-plane series deliberately exported for
// reading rather than paging.
//
// EMPTY, AND THAT IS THE INTENDED STATE. Every series #973 registers has a rule.
// An entry here must carry the argument for why the condition it describes is one
// nobody should be woken for, and the dead-entry check below removes it the moment
// that stops being true.
var copilotDiagnosticOnlySeries = map[string]string{}

// TestEveryRegisteredAgentSeriesIsReadByAnAlert holds direction one: a producer
// with no consumer.
func TestEveryRegisteredAgentSeriesIsReadByAnAlert(t *testing.T) {
	registered := registeredCopilotSeries(t)
	alerting := copilotAlertExprs(t)

	var unread []string
	used := map[string]bool{}
	for _, series := range registered {
		if strings.Contains(alerting, series) {
			continue
		}
		if _, exempt := copilotDiagnosticOnlySeries[series]; exempt {
			used[series] = true
			continue
		}
		unread = append(unread, series)
	}
	if len(unread) > 0 {
		sort.Strings(unread)
		t.Fatalf("agent-plane series with no alert reading them (%d):\n\n  %s\n\n"+
			"The copilot registers these and no Copilot rule's EXPRESSION queries them, so the "+
			"condition each describes is invisible: the agent keeps answering and nothing pages when "+
			"it stops being explainable, stops being grounded, or stops converging. Either write the "+
			"rule, or add the series to copilotDiagnosticOnlySeries with an argument that it is for "+
			"reading rather than paging.", len(unread), strings.Join(unread, "\n  "))
	}

	for series := range copilotDiagnosticOnlySeries {
		if !used[series] {
			t.Fatalf("copilotDiagnosticOnlySeries names %q, which answermetrics.go no longer registers, "+
				"or which an alert now reads. A stale exemption is a hole nobody is watching — remove it.", series)
		}
	}
}

// TestEveryAgentAlertReadsARegisteredSeries holds direction two: a consumer with
// no producer.
//
// THIS IS THE #62 DIRECTION, and it is the one that produces a FALSE sense of
// coverage rather than a missing one. A rule whose series nothing exports never
// fires, so the estate believes it is watched. The check is per-rule against the
// registered set rather than a whole-file substring, because a file that happens
// to mention the series somewhere else would let a broken rule pass.
func TestEveryAgentAlertReadsARegisteredSeries(t *testing.T) {
	registered := registeredCopilotSeries(t)
	inRegistry := map[string]bool{}
	for _, s := range registered {
		inRegistry[s] = true
	}

	rules := copilotRules(t)
	if len(rules) == 0 {
		t.Fatal("found NO Copilot alert rules in operational.rules.yaml — the guard would pass vacuously")
	}
	seriesRe := regexp.MustCompile(`kanz_copilot_[a-z_]+`)
	for _, r := range rules {
		names := seriesRe.FindAllString(r.Expr, -1)
		if len(names) == 0 {
			t.Fatalf("alert %s reads no kanz_copilot_ series in its expression — it cannot be about the "+
				"agent plane, or it is named as if it were", r.Alert)
		}
		for _, n := range names {
			// Histogram rules query the _bucket/_sum/_count derivatives, which the
			// registry never names literally.
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum"), "_count")
			if inRegistry[n] || inRegistry[base] {
				continue
			}
			t.Fatalf("alert %s queries %q, which the copilot registers nowhere. A rule over a series no "+
				"pod exports never fires, so the estate reads as alerted on this condition and is not "+
				"(#62 deleted ten rules in exactly this state). Register the series or delete the rule.",
				r.Alert, n)
		}
	}
}

// TestEveryAgentAlertHasBothAFiringAndAQuietCase is the mutation-resistant half.
//
// A FIRING CASE ALONE PROVES THE RULE CAN FIRE, NOT THAT IT DISCRIMINATES. Every
// ratio rule here divides by a counter an idle deployment never increments, and a
// missing clamp_min turns that into a division that evaluates to nothing — which
// looks exactly like "healthy" and is why the harness must also execute the state
// the deployment spends its time in. The two named groups below are the ones that
// supply seeded-but-flat series to every rule at once; requiring them by name is
// what stops the quiet case being quietly dropped.
func TestEveryAgentAlertHasBothAFiringAndAQuietCase(t *testing.T) {
	b, err := os.ReadFile(copilotTestsRel)
	if err != nil {
		t.Fatalf("read alert tests: %v", err)
	}
	harness := string(b)

	// MATCHED AS A WHOLE LINE, not as a substring. `strings.Contains(harness,
	// "an idle copilot pages nothing")` is satisfied by a group renamed to
	// "an idle copilot pages nothingXX" — a mutation survived this guard for
	// exactly that reason, which is the "a guard that matches prose checks
	// nothing" failure with the prose being the guard's own search term.
	for _, group := range []string{
		"an idle copilot pages nothing",
		"a busy copilot answering cleanly pages nothing",
	} {
		if !strings.Contains(harness, "name: "+group+"\n") {
			t.Fatalf("the alert harness no longer contains the %q case. Every Copilot ratio rule divides "+
				"by a counter that this state leaves at zero, and a harness that only supplies busy "+
				"series cannot tell a guarded denominator from an unfireable rule.", group)
		}
	}

	// AND EVERY RULE MUST APPEAR IN THE HARNESS AT ALL. A new alert with no case
	// is a rule nobody has ever seen evaluate.
	for _, r := range copilotRules(t) {
		if !strings.Contains(harness, "alertname: "+r.Alert) {
			t.Fatalf("alert %s has no case in operational_test.yaml. A rule that has never been executed "+
				"is a claim, not a control.", r.Alert)
		}
	}
}

// TestAnswerOutcomeLabelsAreDerivedFromTheSchema holds the enumeration rule.
func TestAnswerOutcomeLabelsAreDerivedFromTheSchema(t *testing.T) {
	src := readStripped(t, copilotMetricsRel)
	if !strings.Contains(src, "Descriptor().Values()") {
		t.Fatal("answermetrics.go no longer reads the outcome set off the proto descriptor. A label list " +
			"written in this file is a second copy of observation.v1.AgentAnswerOutcome, and the copy is " +
			"what goes stale: a new outcome then ships with no series, every alert on " +
			"kanz_copilot_answers_total silently stops covering it, and the answers land in a label " +
			"nothing reads. #806 and #803 were both this defect. Derive the set; do not re-enumerate it.")
	}
	// The seeded UNSPECIFIED series is what makes that derivation observable —
	// without a rule reading it, a fall-through would still be silent.
	if !strings.Contains(copilotAlertExprs(t), `outcome="unspecified"`) {
		t.Fatal("no Copilot alert reads outcome=\"unspecified\". Seeding the label set from the descriptor " +
			"is only half the control: the series exists so that an outcome the mapping does not " +
			"classify is VISIBLE, and with no rule over it the fall-through is exactly as silent as a " +
			"hand-written label list would have made it.")
	}
}

// TestTheAgentMetricsAreWiredWithoutARecorder holds the wiring rule.
//
// THE SERIES MUST BE REGISTERED OUTSIDE THE PRODUCER BRANCH, and the observer
// attached unconditionally. This is checked structurally — the registration call
// must appear BEFORE the `if answerProducer != nil` it used to sit inside — because
// the failure it prevents is invisible at runtime: a copilot with no broker
// answers questions perfectly well and exports nothing at all to say that nothing
// is being kept.
func TestTheAgentMetricsAreWiredWithoutARecorder(t *testing.T) {
	src := readStripped(t, copilotMainRel)
	reg := strings.Index(src, "newAnswerMetrics()")
	obsv := strings.Index(src, "agent.WithAnswerObserver(")
	branch := strings.Index(src, "if answerProducer != nil")
	switch {
	case reg < 0:
		t.Fatal("main.go never builds the agent-plane metric set — the series are not registered, " +
			"so every Copilot alert queries an empty vector and never fires (#973).")
	case obsv < 0:
		t.Fatal("main.go never attaches agent.WithAnswerObserver — the series are registered and nothing " +
			"feeds them, which reads as a permanently healthy agent plane (#973).")
	case branch < 0:
		t.Fatal("main.go no longer branches on answerProducer; this guard's premise has moved and it " +
			"must be rewritten against whatever now decides whether records are kept.")
	case reg > branch || obsv > branch:
		t.Fatal("newAnswerMetrics or agent.WithAnswerObserver is wired at or after the " +
			"`if answerProducer != nil` branch, which puts the metrics behind the recorder. A " +
			"deployment with COPILOT_NATS_URL unset then keeps no record of any answer AND exports no " +
			"series saying so — the pre-#971 silence with a green dashboard in front of it. " +
			"kanz_copilot_unrecorded_answers_total exists precisely to be non-zero in that state, and " +
			"it cannot be if it is never registered.")
	}
}

// TestContextCoverageIsDerivedFromMeasureStatus holds the #757 rule.
func TestContextCoverageIsDerivedFromMeasureStatus(t *testing.T) {
	src := readStripped(t, copilotGovernPath)
	body := funcBody(t, src, "func (r Reading) Coverage()")
	if !strings.Contains(body, "measureread.StatusMeasured") {
		t.Fatal("Reading.Coverage no longer classifies by measureread.Status. measureread draws the line " +
			"between a measure this plane will state and one it will not exactly once, with a Reason " +
			"attached; re-deriving it here — from a nil Value, from an empty Reason, from anything else " +
			"— is a second implementation of one judgement, and the way the two drift is that a " +
			"WITHHELD measure starts being counted as present. That makes " +
			"kanz_copilot_context_measures_total under-report exactly the gap it exists to show, which " +
			"is #757 with a metric in front of it.")
	}

	// The counts must reach the record too, or the alert can say "a quarter of
	// the book was missing" and nothing can say WHICH answers it was missing from.
	rec := readStripped(t, copilotAgentDir+"/record.go")
	for _, field := range []string{"ContextMeasured:", "ContextWithheld:"} {
		if !strings.Contains(rec, field) {
			t.Fatalf("the answer record no longer carries %s. The metric says how much of the book the "+
				"agent was shown across the estate; the record is the only thing that says it for the "+
				"ONE answer an operator is disputing, which is the question #757 leaves behind.", field)
		}
	}
}

// registeredCopilotSeries returns the metric names answermetrics.go declares.
func registeredCopilotSeries(t *testing.T) []string {
	t.Helper()
	src := readStripped(t, copilotMetricsRel)
	m := regexp.MustCompile(`Name:\s+"(kanz_copilot_[a-z_]+)"`).FindAllStringSubmatch(src, -1)
	if len(m) == 0 {
		t.Fatal("derived NO metric names from answermetrics.go — the guard would pass vacuously")
	}
	// answerRecordsLost is declared in answerrecord.go and registered here; it is
	// part of the same registration and must be covered by the same rules.
	src2 := readStripped(t, "../../services/copilot/cmd/copilot/answerrecord.go")
	m = append(m, regexp.MustCompile(`Name:\s+"(kanz_copilot_[a-z_]+)"`).FindAllStringSubmatch(src2, -1)...)

	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	sort.Strings(out)
	return out
}

type copilotRule struct {
	Alert string `yaml:"alert"`
	Expr  string `yaml:"expr"`
}

// copilotRules parses the Copilot-prefixed alerting rules.
func copilotRules(t *testing.T) []copilotRule {
	t.Helper()
	b, err := os.ReadFile(copilotRulesRel)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []copilotRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse rules: %v", err)
	}
	var out []copilotRule
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if strings.HasPrefix(r.Alert, "Copilot") {
				out = append(out, r)
			}
		}
	}
	return out
}

// copilotAlertExprs concatenates the Copilot rules' EXPRESSIONS.
//
// EXPRESSIONS ONLY. An annotation naming a series is prose, and a guard that
// accepted it would pass on a rule that merely MENTIONS the metric it no longer
// reads — and these descriptions name several series deliberately, to point an
// operator at the next thing to look at. Matching those would make this guard
// green over rules that query nothing.
func copilotAlertExprs(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range copilotRules(t) {
		b.WriteString(r.Expr)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		t.Fatal("found NO Copilot alert expressions — the guard would pass vacuously")
	}
	return b.String()
}

// funcBody returns the source of one function, from its signature to the closing
// brace at column zero.
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("did not find %q — this guard's premise has moved and it must be rewritten", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}
