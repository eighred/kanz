package main

import (
	"strings"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/copilot/internal/agent"
)

// THE AGENT PLANE WAS UNMEASURED (#973).
//
// Every other plane in the estate exports what it did: the OMS counts orders and
// refusals, risk counts denials by reason, accounting counts reconciliation
// breaks by kind. The agent plane counted ONE thing — records it failed to
// publish — and #971 had just made everything else real: refusal outcomes,
// budget exhaustions, grounding verdicts, tool errors, and how much of the book
// the read plane would actually state.
//
// So an estate could not answer, without reading an audit stream by hand:
//
//   - Is the copilot refusing more than it did last week?
//   - Did a model deploy stop the loop converging?
//   - Are answers going out ungrounded?
//   - Is the read plane withholding measures the agent then answered around?
//
// # What is deliberately NOT here
//
// A HALLUCINATION RATE. There is no oracle for it, and a series nobody can
// falsify becomes a number people cite. kanz_copilot_ungrounded_answers_total is
// the checkable half: the grounding gate has already decided whether every stated
// number appeared in a tool result, and that verdict is a fact.
//
// A MODEL DRIFT SERIES. "The model's behaviour changed" is a comparison across
// two populations of answers, not an event a process can emit. The record carries
// the model identity per turn (#971), which is what such a comparison would be
// built from; manufacturing a drift number here would put an unowned judgement on
// a dashboard.
//
// A STALE-CONTEXT RATE. The copilot has no producer for it — its reads are
// point-in-time and the read plane already withholds rather than serving a stale
// number. Proposal staleness is the optimization service's concern and is
// enforced there (#970). Exporting the series here would be a metric with no
// producer, which is the failure #622 named: a gauge that reads healthy because
// nothing ever writes it.

// answerOutcomes counts completed answers by outcome.
//
// SEEDED FROM THE PROTO DESCRIPTOR, not from a list written here. The outcome
// vocabulary belongs to observation.v1.AgentAnswerOutcome, and a hand-maintained
// copy of it is how a new outcome ships with no series and an alert silently
// stops covering it — the same defect #806 and #803 were, one enumeration each.
//
// UNSPECIFIED IS SEEDED TOO, and is not noise: outcomeOf never returns it, so a
// non-zero value there means an outcome fell through the mapping and answers are
// being counted as nothing in particular.

// answerMetrics is the agent-plane series as one set.
//
// A STRUCT AND NOT PACKAGE-LEVEL VARS, because the property that matters here is
// what the set looks like BEFORE the first answer — every label present, every
// value an explicit zero — and package-level collectors carry state between
// tests, so the one assertion worth making is the one they cannot express.
// Grouping them also means the observer feeds all of them or none: a partially
// wired set is the shape that makes a dashboard look like a quiet system.
type answerMetrics struct {
	outcomes         *prometheus.CounterVec
	ungroundedAnswer prometheus.Counter
	ungroundedClaim  prometheus.Counter
	injectionFlagged prometheus.Counter
	turns            prometheus.Histogram
	toolCalls        *prometheus.CounterVec
	contextMeasures  *prometheus.CounterVec
	unrecorded       prometheus.Counter
	recordsLost      prometheus.Counter
}

func newAnswerMetrics() *answerMetrics {
	return &answerMetrics{
		// outcomes counts completed answers by outcome.
		//
		// SEEDED FROM THE PROTO DESCRIPTOR, not from a list written here. The
		// outcome vocabulary belongs to observation.v1.AgentAnswerOutcome, and a
		// hand-maintained copy of it is how a new outcome ships with no series and
		// an alert silently stops covering it — the same defect #806 and #803 were,
		// one enumeration each.
		//
		// UNSPECIFIED IS SEEDED TOO, and is not noise: outcomeOf never returns it,
		// so a non-zero value there means an outcome fell through the mapping and
		// answers are being counted as nothing in particular.
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_copilot_answers_total",
			Help: "Copilot answers by outcome (answered, refused, budget_exhausted, failed). Seeded " +
				"at startup from observation.v1.AgentAnswerOutcome so every outcome reads as an " +
				"explicit zero rather than a missing series (#973).",
		}, []string{"outcome"}),

		// ungroundedAnswer counts answers the grounding gate could not tie to a
		// tool result.
		//
		// THE CHECKABLE HALF OF "DID IT MAKE SOMETHING UP". The gate compares every
		// number the model stated against the values tools actually returned; this
		// counts the times that comparison failed. It is not a hallucination rate
		// and must not be read as one — a model can be wrong in prose with every
		// number grounded.
		ungroundedAnswer: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_copilot_ungrounded_answers_total",
			Help: "Copilot answers carrying at least one stated number that no tool result supports. " +
				"Non-zero means the grounding gate caught the model stating a figure its context " +
				"did not contain (#973).",
		}),

		// ungroundedClaim counts the individual unsupported figures.
		ungroundedClaim: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_copilot_ungrounded_claims_total",
			Help: "Individual stated figures in copilot answers that no tool result supports. Divided " +
				"by kanz_copilot_ungrounded_answers_total it separates one persistently inventive " +
				"answer from a broad grounding failure (#973).",
		}),

		// injectionFlagged counts answers whose tool output tripped the scanner.
		//
		// IT COUNTS ANSWERS, NOT SCANS. The scanner runs per tool result and a
		// single loop can trip it repeatedly on the same poisoned record; counting
		// answers keeps the series a count of affected REQUESTS, which is what an
		// operator is deciding about.
		injectionFlagged: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_copilot_injection_flagged_answers_total",
			Help: "Copilot answers where tool output tripped the prompt-injection scanner. Non-zero " +
				"means content reached the model that looked like an instruction to it (#973).",
		}),

		// turns is how many model turns an answer took.
		//
		// EDGES AT EVERY TURN UP TO THE BUDGET, then past it. agent.DefaultMaxTurns
		// is 8, so single-turn resolution below that is what lets a distribution be
		// seen shifting RIGHT before anything exhausts — the precursor
		// CopilotLoopNotConverging only catches once requests are already failing.
		// The edges beyond 8 are there so a raised budget still resolves rather
		// than collapsing into +Inf.
		turns: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kanz_copilot_answer_turns",
			Help: "Model turns taken per copilot answer. A rising distribution is the earliest sign " +
				"of a model that has stopped converging (#973).",
			Buckets: []float64{1, 2, 3, 4, 5, 6, 8, 10, 15},
		}),

		// toolCalls counts tool invocations by result.
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_copilot_tool_calls_total",
			Help: "Tool invocations made by the copilot agent, by result. A rising error share is a " +
				"read plane degrading under an agent that keeps asking (#973).",
		}, []string{"result"}),

		// contextMeasures counts what the read plane put in front of the model.
		//
		// THIS IS THE #757 SERIES. A measure whose integrity record was stripped
		// once reached an agent as a computed zero; the read plane withholds now
		// instead. But an answer hedged because two of three measures were
		// UNAVAILABLE and one hedged for no reason looked identical from outside,
		// and a rising withheld share is a data-quality failure being absorbed
		// silently by an agent that hedges.
		//
		// COUNTS AND NOT MEASURE NAMES. The measure vocabulary belongs to the risk
		// engine; a label fed by another component's vocabulary is unbounded by
		// construction.
		contextMeasures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_copilot_context_measures_total",
			Help: "Risk measures the read plane put in front of the copilot model, by whether it " +
				"would state a number for them. A rising withheld share means answers are being " +
				"composed against a book with holes (#973).",
		}, []string{"status"}),

		// unrecorded counts answers that reached no audit record, for any reason.
		//
		// A SUPERSET OF kanz_copilot_answer_records_lost_total, AND THAT IS THE
		// POINT. The lost counter fires when a publish fails. It CANNOT fire when
		// no recorder is configured at all — the pre-#971 state — so a deployment
		// that keeps no record of anything exports a permanent, healthy-looking
		// zero. "Nothing configured" and "checked, and fine" must never look the
		// same, and this is the series that tells them apart.
		unrecorded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_copilot_unrecorded_answers_total",
			Help: "Copilot answers that reached no audit record, whether because publishing failed " +
				"or because no recorder is configured. Non-zero means answers exist that cannot " +
				"be reconstructed (#973).",
		}),

		// recordsLost is #971's counter, owned here so the two "an answer was not
		// kept" series are registered together. Registering one without the other
		// is how an operator ends up comparing a rising counter against a series
		// that does not exist.
		recordsLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_copilot_answer_records_lost_total",
			Help: "Copilot answers that were produced and whose record could not be published. " +
				"Non-zero means an answer exists that cannot be reconstructed: what the model was " +
				"shown, which model answered and whether the answer was grounded are " +
				"unrecoverable for it (#971).",
		}),
	}
}

// register registers the set and seeds every label.
//
// SEEDING BEFORE THE FIRST ANSWER is what makes "no refusals" a readable zero
// instead of an absent series. An alert arm over a series that does not exist
// evaluates to nothing, so a rule written to catch a refusal spike would also be
// silent in the state where the copilot has refused nothing — and silent, on a
// counter, is indistinguishable from healthy (#622).
func (m *answerMetrics) register(r prometheus.Registerer) {
	r.MustRegister(m.outcomes, m.ungroundedAnswer, m.ungroundedClaim, m.injectionFlagged,
		m.turns, m.toolCalls, m.contextMeasures, m.unrecorded, m.recordsLost)
	for _, o := range answerOutcomeLabels() {
		m.outcomes.WithLabelValues(o)
	}
	m.toolCalls.WithLabelValues("ok")
	m.toolCalls.WithLabelValues("error")
	m.contextMeasures.WithLabelValues("measured")
	m.contextMeasures.WithLabelValues("withheld")
}

// answerOutcomeLabels returns every outcome the wire enum declares.
//
// READ OFF THE DESCRIPTOR so the set cannot drift from the schema. A list written
// here would be a second copy of the vocabulary, and the copy is what goes stale.
func answerOutcomeLabels() []string {
	vals := observationpb.AgentAnswerOutcome(0).Descriptor().Values()
	out := make([]string, 0, vals.Len())
	for i := 0; i < vals.Len(); i++ {
		out = append(out, outcomeLabel(observationpb.AgentAnswerOutcome(vals.Get(i).Number())))
	}
	return out
}

// outcomeLabel renders an outcome as a metric label.
func outcomeLabel(o observationpb.AgentAnswerOutcome) string {
	return strings.ToLower(strings.TrimPrefix(o.String(), "AGENT_ANSWER_OUTCOME_"))
}

// observe records one completed answer against the agent-plane series.
//
// IT IS THE WHOLE OBSERVER, so every series moves on the same event and none can
// silently stop being fed while the others keep reporting.
func (m *answerMetrics) observe(obs agent.Observation) {
	m.outcomes.WithLabelValues(outcomeLabel(obs.Outcome)).Inc()
	m.turns.Observe(float64(obs.Turns))
	if n := obs.ToolCalls - obs.ToolErrors; n > 0 {
		m.toolCalls.WithLabelValues("ok").Add(float64(n))
	}
	if obs.ToolErrors > 0 {
		m.toolCalls.WithLabelValues("error").Add(float64(obs.ToolErrors))
	}
	if obs.Measured > 0 {
		m.contextMeasures.WithLabelValues("measured").Add(float64(obs.Measured))
	}
	if obs.Withheld > 0 {
		m.contextMeasures.WithLabelValues("withheld").Add(float64(obs.Withheld))
	}
	// GROUNDED IS ONLY MEANINGFUL ON AN ANSWER. A refusal states no numbers, so
	// the gate leaves it grounded by construction; a FAILED loop carries the ZERO
	// VALUE of Grounded rather than a verdict, and counting that would fill the
	// series with transport faults.
	if obs.Outcome == observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED && !obs.Grounded {
		m.ungroundedAnswer.Inc()
		m.ungroundedClaim.Add(float64(obs.UngroundedClaims))
	}
	if obs.InjectionFlagged {
		m.injectionFlagged.Inc()
	}
	if !obs.RecordKept {
		m.unrecorded.Inc()
	}
}
