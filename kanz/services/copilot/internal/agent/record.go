package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

// AN AGENT ANSWER MUST BE RECONSTRUCTABLE (#971).
//
// Every AUTHORIZATION decision the copilot makes was already recorded — the
// AuditedAuthorizer turns each allow and deny into an observation.v1.DecisionLog
// (#352). WHAT THE MODEL WAS SHOWN WAS NOT. So the platform could say "this
// principal was permitted to read this resource" and could not say "and here is
// what it was shown, by which model, and what it concluded".
//
// When an operator says "the copilot told me the book was flat in EURUSD and it
// was not", four different faults were indistinguishable: a stale or partial
// retrieval, a measure that arrived with a degraded integrity record (#757's
// shape, where DV01 = 0 reached an agent as if it were a computed zero), a model
// that was a different version than the one deployed today, or a model producing
// what its context did not support. Four faults, four owners, one silence.
//
// # What is recorded, and what deliberately is not
//
// References, not payloads. The retrieval set is derived from tenant-scoped state
// and may carry position-level detail; copying it here would put a second copy of
// the book on the observation stream under different isolation, and "was this
// measure degraded when the agent saw it" is answerable from a reference plus a
// re-fetch.
//
// The question and answer TEXT are absent for the same reason. Their digests are
// recorded so a transcript the caller holds can be matched to this record beyond
// doubt; the text belongs to that transcript. What this record establishes is
// what the agent was SHOWN and what it CONCLUDED — the two things nothing else
// keeps.

// AnswerRecorder is where an answer record goes. It is a one-method seam so this
// package can be tested without a broker, and so the composition root decides
// what "recorded" means — the same shape consume.Publisher and custody.Publisher
// take.
type AnswerRecorder interface {
	RecordAnswer(ctx context.Context, rec *observationpb.AgentAnswerRecord) error
}

// WithRecorder attaches the answer recorder.
//
// A NIL RECORDER IS ANNOUNCED, NOT TOLERATED SILENTLY. Without one the copilot
// answers and nothing keeps what it was shown — which is the pre-#971 state, and
// it looks identical from the outside to a healthy one. The composition root says
// so at startup rather than letting the estate discover it when somebody asks why
// an answer cannot be explained.
func WithRecorder(r AnswerRecorder) Option { return func(a *Agent) { a.recorder = r } }

// WithRecordObserver is called when a record could not be published.
//
// A LOST RECORD IS COUNTED, NEVER SILENT. Recording is derived from the answer
// and must not fail the answer — refusing to answer because the audit stream is
// down would turn a broker blip into a copilot outage — but "we answered and kept
// no record of it" is exactly the state this issue exists to abolish, so it gets
// a counter rather than a swallowed error.
func WithRecordObserver(fn func(err error)) Option {
	return func(a *Agent) { a.onRecordLost = fn }
}

// WithLogger attaches the logger the record path reports on.
func WithLogger(l *slog.Logger) Option { return func(a *Agent) { a.log = l } }

// WithClock injects the record's timestamp source.
func WithClock(now func() time.Time) Option { return func(a *Agent) { a.now = now } }

// Option configures an Agent.
type Option func(*Agent)

// digest renders a stable, non-reversible identifier for a piece of text.
//
// TRUNCATED TO 16 BYTES, which is 128 bits — far past any collision an operator
// could encounter, and short enough to read in a log line. It is an IDENTIFIER
// and not a commitment: nothing here is verifying an adversary's claim about the
// text, only matching a record to a transcript.
//
// AN EMPTY STRING DIGESTS TO EMPTY rather than to the hash of nothing. "There was
// no answer text" and "the answer text was the empty string" are different facts,
// and a refused answer legitimately has none — encoding both as e3b0c442… would
// make a refusal look like it carried content.
func digest(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// contextDigest is a digest over everything the model was shown, in the order it
// was shown — so two answers built from the same material are recognisable as
// such, and a changed context is detectable without storing either.
//
// THE CITATIONS' IDENTITIES, NOT THEIR VALUES. A digest over values would change
// when a measure was merely recomputed to the same number in a different
// representation, and would not change when the same event id started resolving
// to a different lineage node — which is the drift a reader actually cares about.
func contextDigest(cites []retrieval.Citation) string {
	if len(cites) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range cites {
		b.WriteString(c.SourceEventID)
		b.WriteByte('|')
		b.WriteString(c.PortfolioID)
		b.WriteByte('|')
		b.WriteString(c.AsOf)
		b.WriteByte('|')
		b.WriteString(c.LineageNode)
		b.WriteByte('\n')
	}
	return digest(b.String())
}

// references renders the retrieval manifest.
func references(cites []retrieval.Citation) []*observationpb.RetrievedReference {
	if len(cites) == 0 {
		return nil
	}
	out := make([]*observationpb.RetrievedReference, 0, len(cites))
	for _, c := range cites {
		out = append(out, &observationpb.RetrievedReference{
			SourceEventId: c.SourceEventID,
			PortfolioId:   c.PortfolioID,
			AsOf:          c.AsOf,
			LineageNode:   c.LineageNode,
		})
	}
	return out
}

// outcomeOf maps an answer to its wire outcome.
func outcomeOf(ans Answer, failed bool) observationpb.AgentAnswerOutcome {
	switch {
	case failed:
		return observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_FAILED
	case ans.Refused:
		return observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_REFUSED
	case ans.BudgetExhausted:
		return observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_BUDGET_EXHAUSTED
	default:
		return observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED
	}
}

// Observation is what one completed answer says about the agent plane (#973).
//
// IT IS THE SAME FACTS AS THE RECORD, IN COUNTABLE FORM. The record explains ONE
// answer to a reader who already has a complaint; the observation is how the
// estate notices there is something to complain about — a refusal rate that
// doubled overnight, a loop that stopped converging, a read plane that started
// withholding. Deriving the second from the first afterwards would mean reading
// the audit stream to alert on it, which makes the alerting path depend on a
// stream whose whole purpose is to be durable rather than prompt.
//
// NO PRINCIPAL, NO TENANT, NO ANSWER ID. This crosses into a metrics registry,
// and a per-tenant label on an estate-wide series is unbounded cardinality plus a
// disclosure surface — one tenant's question volume readable from another's
// dashboard. Isolation-bearing detail stays on the record, which is tenant-scoped
// by construction.
type Observation struct {
	Outcome          observationpb.AgentAnswerOutcome
	Grounded         bool
	UngroundedClaims int
	InjectionFlagged bool
	Turns            int
	ToolCalls        int
	ToolErrors       int

	// Measured and Withheld count what the read plane put in front of the model.
	Measured int
	Withheld int

	// RecordKept is whether this answer's audit record was published. False means
	// the estate answered a question it cannot now explain — the same event
	// answerRecordsLost counts, carried here so the two cannot disagree.
	RecordKept bool
}

// WithAnswerObserver is called once for every completed answer, whatever its
// outcome.
//
// IT FIRES WITHOUT A RECORDER. Metrics and the audit record are separate
// obligations: a deployment with no broker still has to be observable, and
// wiring the counters through the recorder would make an estate lose its refusal
// rate the moment the audit stream went down — losing the signal exactly when it
// matters most.
func WithAnswerObserver(fn func(Observation)) Option {
	return func(a *Agent) { a.onAnswer = fn }
}

// record builds and publishes the answer record, and reports the observation.
//
// IT IS EMITTED FOR EVERY OUTCOME, including a refusal, a budget exhaustion and a
// transport failure. An answer the model declined is a fact about the agent worth
// keeping — the same argument ProposalMaterialized makes for a materialization
// that published nothing — and a loop that exhausted its budget is the earliest
// sign of a model that has stopped converging. Recording only the successes would
// leave exactly the failures unexplained.
func (a *Agent) record(ctx context.Context, p *auth.Principal, question string, ans Answer, trace loopTrace, failed bool) {
	obs := Observation{
		Outcome: outcomeOf(ans, failed), Grounded: ans.Grounded, UngroundedClaims: len(ans.Ungrounded),
		InjectionFlagged: ans.InjectionFlagged, Turns: trace.turns, ToolCalls: trace.toolCalls,
		ToolErrors: trace.toolErrors, Measured: trace.measured, Withheld: trace.unavailable,
		RecordKept: true,
	}
	if a.recorder == nil {
		// NO RECORDER IS NOT A KEPT RECORD. Reporting RecordKept here would make a
		// deployment that keeps nothing at all indistinguishable from one whose
		// every record landed, which is the pre-#971 silence with a green metric
		// in front of it.
		obs.RecordKept = false
		a.observe(obs)
		return
	}
	rec := &observationpb.AgentAnswerRecord{
		AnswerId:         trace.answerID,
		Agent:            agentName,
		PrincipalSubject: p.Subject,
		TenantId:         p.Tenant,
		Models:           trace.models,
		Retrieved:        references(ans.Citations),
		ContextDigest:    contextDigest(ans.Citations),
		QuestionDigest:   digest(question),
		AnswerDigest:     digest(ans.Text),
		Outcome:          outcomeOf(ans, failed),
		Grounded:         ans.Grounded,
		UngroundedClaims: ans.Ungrounded,
		InjectionFlagged: ans.InjectionFlagged,
		Turns:            uint32(trace.turns),
		ToolCalls:        uint32(trace.toolCalls),
		ToolErrors:       uint32(trace.toolErrors),
		ContextMeasured:  uint32(trace.measured),
		ContextWithheld:  uint32(trace.unavailable),
		AnsweredAt:       timestamppb.New(a.clock()),
	}
	if err := a.recorder.RecordAnswer(ctx, rec); err != nil {
		obs.RecordKept = false
		a.observe(obs)
		if a.onRecordLost != nil {
			a.onRecordLost(err)
		}
		a.logger().Error("copilot: an answer was produced and NOT recorded — what the model was "+
			"shown, which model answered and whether the answer was grounded are unrecoverable "+
			"for this request (#971)",
			"answer_id", trace.answerID, "principal", p.Subject, "err", err)
		return
	}
	a.observe(obs)
}

// observe reports one answer to the metrics seam.
func (a *Agent) observe(obs Observation) {
	if a.onAnswer == nil {
		return
	}
	a.onAnswer(obs)
}

// agentName is the surface this record came from. It is a constant rather than a
// parameter because a copilot that could name itself something else would make an
// estate-wide refusal rate uninterpretable; the MCP read plane is a second agent
// and will carry its own.
const agentName = "copilot"

// newAnswerID derives this answer's identity.
//
// TIME PLUS RANDOMNESS, not a hash of the question. Two identical questions asked
// a minute apart are two answers, and a content-derived id would collapse them —
// which is exactly the pair a reader most wants to compare when a model's
// behaviour has changed under them.
func newAnswerID(now time.Time) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A DEGRADED ID IS BETTER THAN NO RECORD. Losing the answer record because
		// the entropy source hiccuped would trade a small collision risk for the
		// whole point of the record; the timestamp still orders and nearly
		// separates them.
		return "ans-" + now.UTC().Format("20060102T150405.000000000Z")
	}
	return "ans-" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// loopTrace is what the tool-use loop observed about itself.
type loopTrace struct {
	answerID   string
	models     []string
	turns      int
	toolCalls  int
	toolErrors int

	// measured and unavailable count what the read plane put in front of the model
	// across the whole loop, by whether it would state a number (#973).
	measured    int
	unavailable int
}

// clock resolves the timestamp source.
func (a *Agent) clock() time.Time {
	if a.now == nil {
		return time.Now().UTC()
	}
	return a.now().UTC()
}

// logger resolves the logger.
func (a *Agent) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}
