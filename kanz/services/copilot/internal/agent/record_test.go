package agent

// Answer-record tests (#971).
//
// The record exists so an answer can be RECONSTRUCTED after the fact, so each
// test below names the question a reader would be unable to answer without it.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/tools"
)

var recT0 = time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)

// capturingRecorder keeps what was recorded.
type capturingRecorder struct {
	records []*observationpb.AgentAnswerRecord
	err     error
}

func (c *capturingRecorder) RecordAnswer(_ context.Context, rec *observationpb.AgentAnswerRecord) error {
	if c.err != nil {
		return c.err
	}
	c.records = append(c.records, rec)
	return nil
}

func (c *capturingRecorder) only(t *testing.T) *observationpb.AgentAnswerRecord {
	t.Helper()
	if len(c.records) != 1 {
		t.Fatalf("recorded %d answers, want exactly 1", len(c.records))
	}
	return c.records[0]
}

// scriptedModel returns a fixed sequence of responses.
type scriptedModel struct {
	turns []llm.Response
	i     int
	err   error
}

func (m *scriptedModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	if m.err != nil {
		return llm.Response{}, m.err
	}
	if m.i >= len(m.turns) {
		return llm.Response{Text: "done", StopReason: llm.StopEndTurn, Model: "opus-x/18"}, nil
	}
	r := m.turns[m.i]
	m.i++
	return r, nil
}

func recPrincipal() *auth.Principal {
	return &auth.Principal{Subject: "alice@desk", Tenant: "acme"}
}

// agentOver builds an agent over a REAL tool registry. A nil registry would
// nil-panic in Ask on the first Defs() call — these tests are about the record,
// not about tolerating an unwired agent, and the composition root is what refuses
// that.
func agentOver(t *testing.T, m llm.Model, rec AnswerRecorder, opts ...Option) *Agent {
	t.Helper()
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}),
		nil, "copilot", slog.New(slog.NewTextHandler(discard{}, nil)))
	reg := tools.NewRegistry(authz, governed.NewStubClient(), retrieval.IdentityCatalog{}, nil)
	base := []Option{WithRecorder(rec), WithClock(func() time.Time { return recT0 })}
	return New(m, reg, append(base, opts...)...)
}

// AN ANSWER IS RECORDED WITH WHAT PRODUCED IT. Without this the platform could
// say a principal was PERMITTED to read a resource and could not say which model
// answered or what it concluded.
func TestAnAnsweredQuestionIsRecorded(t *testing.T) {
	rec := &capturingRecorder{}
	m := &scriptedModel{turns: []llm.Response{
		{Text: "the book is flat", StopReason: llm.StopEndTurn, Model: "opus-x/18"},
	}}
	ans, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), "how is the book?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got := rec.only(t)

	if got.GetOutcome() != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED {
		t.Fatalf("outcome = %s, want ANSWERED", got.GetOutcome())
	}
	if got.GetPrincipalSubject() != "alice@desk" || got.GetTenantId() != "acme" {
		t.Fatalf("principal = %s/%s, want alice@desk/acme — taken from the authenticated principal",
			got.GetPrincipalSubject(), got.GetTenantId())
	}
	if len(got.GetModels()) != 1 || got.GetModels()[0] != "opus-x/18" {
		t.Fatalf("models = %v, want [opus-x/18] — 'which model actually answered' is the question "+
			"only the response can settle", got.GetModels())
	}
	if got.GetAnswerId() == "" {
		t.Fatal("no answer_id — nothing else about this answer can correlate")
	}
	if got.GetAnsweredAt().AsTime() != recT0 {
		t.Fatalf("answered_at = %s, want %s", got.GetAnsweredAt().AsTime(), recT0)
	}
	if got.GetTurns() != 1 {
		t.Fatalf("turns = %d, want 1", got.GetTurns())
	}
	_ = ans
}

// THE TEXT IS NOT RECORDED AND ITS DIGEST IS. The retrieval set is tenant-scoped
// and may carry position-level detail; the digest matches this record to a
// transcript the caller holds without putting a second copy of the book on the
// observation stream.
func TestTheRecordCarriesDigestsAndNotText(t *testing.T) {
	rec := &capturingRecorder{}
	const question = "what is my EURUSD exposure?"
	const answer = "flat, per the 14:00 exposure set"
	m := &scriptedModel{turns: []llm.Response{{Text: answer, StopReason: llm.StopEndTurn, Model: "opus-x/18"}}}
	if _, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), question); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got := rec.only(t)

	blob := got.String()
	if strings.Contains(blob, "EURUSD") || strings.Contains(blob, "flat, per the") {
		t.Fatalf("the record carries question or answer TEXT:\n%s\n\nIt must carry digests only — "+
			"the text belongs to the caller's transcript, and copying it here puts tenant data on "+
			"a second stream", blob)
	}
	if got.GetQuestionDigest() == "" || got.GetAnswerDigest() == "" {
		t.Fatal("a digest is missing — the record cannot be matched to a transcript")
	}
	if got.GetQuestionDigest() == got.GetAnswerDigest() {
		t.Fatal("the question and answer digest to the same value")
	}
	// Deterministic: the same text digests the same way, so two records over the
	// same material are recognisable as such.
	if digest(question) != got.GetQuestionDigest() {
		t.Fatal("the question digest is not reproducible from the question")
	}
}

// A REFUSAL IS A FACT ABOUT THE AGENT AND IS RECORDED. Recording only the
// successes would leave exactly the interesting cases unexplained.
func TestARefusedAnswerIsRecorded(t *testing.T) {
	rec := &capturingRecorder{}
	m := &scriptedModel{turns: []llm.Response{
		{Text: "I can't help with that", StopReason: llm.StopRefusal, Model: "opus-x/18"},
	}}
	if _, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), "q"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got := rec.only(t)
	if got.GetOutcome() != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_REFUSED {
		t.Fatalf("outcome = %s, want REFUSED", got.GetOutcome())
	}
}

// A LOOP THAT NEVER CONVERGED IS NOT A REFUSAL. They are different facts with
// different owners — the first is the model working, the second is the earliest
// sign of one that has stopped — and they previously returned the same shape.
func TestAnExhaustedBudgetIsItsOwnOutcome(t *testing.T) {
	rec := &capturingRecorder{}
	// Every turn asks for a tool, so the loop never reaches a final answer.
	loop := llm.Response{
		StopReason: llm.StopToolUse, Model: "opus-x/18",
		ToolCalls: []llm.ToolCall{{ID: "t1", Name: "nope"}},
	}
	turns := make([]llm.Response, DefaultMaxTurns)
	for i := range turns {
		turns[i] = loop
	}
	a := agentOver(t, &scriptedModel{turns: turns}, rec)
	ans, err := a.Ask(context.Background(), recPrincipal(), "q")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ans.BudgetExhausted {
		t.Fatal("the answer does not report BudgetExhausted")
	}
	if ans.Refused {
		t.Fatal("an exhausted budget was reported as a refusal — different fact, different owner")
	}
	got := rec.only(t)
	if got.GetOutcome() != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_BUDGET_EXHAUSTED {
		t.Fatalf("outcome = %s, want BUDGET_EXHAUSTED", got.GetOutcome())
	}
	if got.GetTurns() != uint32(DefaultMaxTurns) {
		t.Fatalf("turns = %d, want %d — a rising turn count is the earliest sign of a model that "+
			"has stopped converging", got.GetTurns(), DefaultMaxTurns)
	}
}

// A LOOP THAT COULD NOT RUN IS RECORDED TOO. Otherwise the one case where an
// operator most wants to know what happened leaves nothing behind.
func TestAFailedLoopIsRecorded(t *testing.T) {
	rec := &capturingRecorder{}
	m := &scriptedModel{err: errors.New("provider unreachable")}
	if _, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), "q"); err == nil {
		t.Fatal("Ask returned nil for a failing model")
	}
	got := rec.only(t)
	if got.GetOutcome() != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_FAILED {
		t.Fatalf("outcome = %s, want FAILED", got.GetOutcome())
	}
}

// A LOST RECORD IS COUNTED AND NEVER SILENT — and it does not fail the answer.
// Refusing to answer because the audit stream is down would turn a broker blip
// into a copilot outage; keeping no record and saying nothing is the state this
// issue abolishes.
func TestALostRecordIsObservedAndDoesNotFailTheAnswer(t *testing.T) {
	rec := &capturingRecorder{err: errors.New("broker down")}
	var lost int
	m := &scriptedModel{turns: []llm.Response{{Text: "ok", StopReason: llm.StopEndTurn, Model: "opus-x/18"}}}
	a := agentOver(t, m, rec, WithRecordObserver(func(error) { lost++ }))

	ans, err := a.Ask(context.Background(), recPrincipal(), "q")
	if err != nil {
		t.Fatalf("a failed RECORD failed the ANSWER: %v — the answer must survive an audit-stream "+
			"outage", err)
	}
	if ans.Text != "ok" {
		t.Fatalf("answer = %q, want ok", ans.Text)
	}
	if lost != 1 {
		t.Fatalf("record losses observed = %d, want 1 — an unrecorded answer must be counted", lost)
	}
}

// NO RECORDER RECORDS NOTHING AND DOES NOT PANIC. The composition root reports
// that posture at startup; the agent must not crash on it.
func TestNoRecorderIsSafe(t *testing.T) {
	m := &scriptedModel{turns: []llm.Response{{Text: "ok", StopReason: llm.StopEndTurn}}}
	if _, err := New(m, tools.NewRegistry(
		auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(&auth.Policy{}), nil, "copilot",
			slog.New(slog.NewTextHandler(discard{}, nil))),
		governed.NewStubClient(), retrieval.IdentityCatalog{}, nil)).Ask(context.Background(), recPrincipal(), "q"); err != nil {
		t.Fatalf("Ask with no recorder: %v", err)
	}
}

// THE MODEL LIST LENGTH MATCHES THE TURN COUNT, including a turn whose provider
// reported no identity. Omitting the empty entry would silently shorten the list
// and make it disagree with turns — which is the one thing that would make the
// record unreadable.
func TestATurnWithNoModelIdentityStillContributesAnEntry(t *testing.T) {
	rec := &capturingRecorder{}
	m := &scriptedModel{turns: []llm.Response{
		{Text: "", StopReason: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "t1", Name: "nope"}}}, // no Model
		{Text: "done", StopReason: llm.StopEndTurn, Model: "opus-x/18"},
	}}
	if _, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), "q"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got := rec.only(t)
	if int(got.GetTurns()) != len(got.GetModels()) {
		t.Fatalf("turns = %d but models has %d entries — an omitted identity makes the list "+
			"disagree with the loop", got.GetTurns(), len(got.GetModels()))
	}
	if got.GetModels()[0] != "" || got.GetModels()[1] != "opus-x/18" {
		t.Fatalf("models = %v, want [\"\", opus-x/18]", got.GetModels())
	}
}

// THE RETRIEVAL MANIFEST IS REFERENCES, AND THE CONTEXT DIGEST FOLLOWS IT. Two
// answers over the same material are recognisable; a changed context is
// detectable without storing either.
func TestTheManifestIsReferencesAndTheDigestFollowsThem(t *testing.T) {
	a := []retrieval.Citation{{SourceEventID: "e1", PortfolioID: "PF1", AsOf: "2026-09-02T14:00:00Z", LineageNode: "n1"}}
	b := []retrieval.Citation{{SourceEventID: "e1", PortfolioID: "PF1", AsOf: "2026-09-02T14:00:00Z", LineageNode: "n2"}}

	if contextDigest(a) == "" {
		t.Fatal("a non-empty citation set digests to empty")
	}
	// Determinism against a SEPARATELY CONSTRUCTED equal set, not against itself —
	// comparing one call to another call on the same slice proves nothing a
	// compiler would not already guarantee.
	aAgain := []retrieval.Citation{{SourceEventID: "e1", PortfolioID: "PF1", AsOf: "2026-09-02T14:00:00Z", LineageNode: "n1"}}
	if contextDigest(a) != contextDigest(aAgain) {
		t.Fatal("two equal citation sets digested differently — two answers over the same material " +
			"would not be recognisable as such")
	}
	if contextDigest(a) == contextDigest(b) {
		t.Fatal("the same event id resolving to a DIFFERENT lineage node produced the same digest " +
			"— that is precisely the drift a reader cares about")
	}
	if contextDigest(nil) != "" {
		t.Fatal("an empty citation set must digest to empty, not to the hash of nothing")
	}

	refs := references(a)
	if len(refs) != 1 {
		t.Fatalf("%d references, want 1", len(refs))
	}
	r := refs[0]
	if r.GetSourceEventId() != "e1" || r.GetPortfolioId() != "PF1" || r.GetLineageNode() != "n1" {
		t.Fatalf("reference = %+v, want the citation's identity", r)
	}
	if r.GetAsOf() != "2026-09-02T14:00:00Z" {
		t.Fatalf("as_of = %q — it is the half that answers 'was this stale when the agent saw it'",
			r.GetAsOf())
	}
}

// AN EMPTY ANSWER DIGESTS TO EMPTY. "There was no answer text" and "the answer
// text was the empty string" are different facts, and a refusal legitimately has
// none — encoding both as the hash of nothing would make a refusal look like it
// carried content.
func TestAnEmptyStringDigestsToEmpty(t *testing.T) {
	if digest("") != "" {
		t.Fatalf("digest(\"\") = %q, want empty", digest(""))
	}
	if digest("x") == "" {
		t.Fatal("a non-empty string digested to empty")
	}
}

// TWO ANSWERS TO THE SAME QUESTION ARE TWO RECORDS. A content-derived id would
// collapse them — and that pair is exactly what a reader compares when a model's
// behaviour has changed under them.
func TestTwoIdenticalQuestionsGetDistinctAnswerIDs(t *testing.T) {
	rec := &capturingRecorder{}
	for i := 0; i < 2; i++ {
		m := &scriptedModel{turns: []llm.Response{{Text: "same", StopReason: llm.StopEndTurn, Model: "opus-x/18"}}}
		if _, err := agentOver(t, m, rec).Ask(context.Background(), recPrincipal(), "identical"); err != nil {
			t.Fatalf("Ask %d: %v", i, err)
		}
	}
	if len(rec.records) != 2 {
		t.Fatalf("recorded %d answers, want 2", len(rec.records))
	}
	if rec.records[0].GetAnswerId() == rec.records[1].GetAnswerId() {
		t.Fatalf("two answers share an id (%s) — the pair a reader most wants to compare would "+
			"collapse", rec.records[0].GetAnswerId())
	}
	// The question digest, by contrast, MUST match: that is how they are known to
	// be the same question.
	if rec.records[0].GetQuestionDigest() != rec.records[1].GetQuestionDigest() {
		t.Fatal("the same question digested differently — two answers to one question would be " +
			"unlinkable")
	}
}
