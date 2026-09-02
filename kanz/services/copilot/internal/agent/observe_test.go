package agent

// Agent-plane observation tests (#973).
//
// #971 made an answer RECONSTRUCTABLE. These cover the second obligation: the
// same facts in countable form, so the estate notices there is something to
// reconstruct. Each test names the state that would otherwise be invisible.

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/internal/measureread"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/tools"
)

// testRegistry builds the real tool registry agentOver uses, for the one case
// that must construct an Agent with NO recorder at all.
func testRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}),
		nil, "copilot", slog.New(slog.NewTextHandler(discard{}, nil)))
	return tools.NewRegistry(authz, governed.NewStubClient(), retrieval.IdentityCatalog{}, nil)
}

// observationFieldNames reflects Observation's exported field names.
func observationFieldNames(o Observation) []string {
	tp := reflect.TypeOf(o)
	out := make([]string, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		out = append(out, tp.Field(i).Name)
	}
	return out
}

// capturingObserver keeps every observation reported.
type capturingObserver struct{ seen []Observation }

func (c *capturingObserver) fn() func(Observation) {
	return func(o Observation) { c.seen = append(c.seen, o) }
}

func (c *capturingObserver) only(t *testing.T) Observation {
	t.Helper()
	if len(c.seen) != 1 {
		t.Fatalf("observed %d answers, want exactly 1", len(c.seen))
	}
	return c.seen[0]
}

// EVERY ANSWER IS OBSERVED, WHATEVER ITS OUTCOME. Counting only the successes
// would leave the estate blind to exactly the states worth alerting on — the
// refusals, the exhausted loops, the failures — which is the shape #971 already
// rejected for the record and which a metrics path can regress to independently.
func TestEveryOutcomeIsObserved(t *testing.T) {
	cases := []struct {
		name  string
		model *scriptedModel
		want  observationpb.AgentAnswerOutcome
	}{
		{
			name:  "answered",
			model: &scriptedModel{turns: []llm.Response{{Text: "flat", StopReason: llm.StopEndTurn, Model: "m/1"}}},
			want:  observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		},
		{
			name:  "failed",
			model: &scriptedModel{err: errors.New("upstream gone")},
			want:  observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_FAILED,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := &capturingObserver{}
			a := agentOver(t, tc.model, &capturingRecorder{}, WithAnswerObserver(obs.fn()))
			_, _ = a.Ask(context.Background(), recPrincipal(), "how is the book?")
			if got := obs.only(t).Outcome; got != tc.want {
				t.Fatalf("outcome = %s, want %s", got, tc.want)
			}
		})
	}
}

// THE OBSERVER FIRES WITHOUT A RECORDER, and that is the whole point of the seam.
//
// A deployment with COPILOT_NATS_URL unset answers questions perfectly well and
// keeps nothing. Before #973 it also exported nothing to say so, because the
// series were registered inside the branch that builds the recorder — so the
// state where NOTHING is kept scraped identically to the state where every record
// landed. If this ever regresses, that silence comes back with a green dashboard
// in front of it.
func TestAnswersAreObservedWithNoRecorderConfigured(t *testing.T) {
	obs := &capturingObserver{}
	m := &scriptedModel{turns: []llm.Response{{Text: "flat", StopReason: llm.StopEndTurn, Model: "m/1"}}}
	a := New(m, testRegistry(t), WithAnswerObserver(obs.fn()))

	if _, err := a.Ask(context.Background(), recPrincipal(), "how is the book?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got := obs.only(t)
	if got.RecordKept {
		t.Fatal("RecordKept = true with NO recorder configured. Nothing kept this answer, so reporting " +
			"it as kept makes a deployment that records nothing at all indistinguishable from one " +
			"whose every record landed — which is the pre-#971 silence, now with a healthy metric " +
			"in front of it (#973).")
	}
	if got.Outcome != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED {
		t.Fatalf("outcome = %s, want ANSWERED — the answer itself must be unaffected by the absence "+
			"of a recorder", got.Outcome)
	}
}

// A FAILED PUBLISH IS THE OTHER WAY TO REACH "NOT KEPT", and it must report the
// same way. The two causes are told apart by kanz_copilot_answer_records_lost_total,
// which only the publish path increments — but the superset series has to move in
// BOTH, or the critical rule misses the case it was written for.
func TestALostRecordIsObservedAsNotKept(t *testing.T) {
	obs := &capturingObserver{}
	rec := &capturingRecorder{err: errors.New("broker down")}
	m := &scriptedModel{turns: []llm.Response{{Text: "flat", StopReason: llm.StopEndTurn, Model: "m/1"}}}
	a := agentOver(t, m, rec, WithAnswerObserver(obs.fn()))

	if _, err := a.Ask(context.Background(), recPrincipal(), "how is the book?"); err != nil {
		t.Fatalf("Ask must not fail because the audit stream is down: %v", err)
	}
	if obs.only(t).RecordKept {
		t.Fatal("RecordKept = true after the publish failed — the answer exists and its record does not")
	}
}

// A KEPT RECORD REPORTS AS KEPT. Without this the two tests above pass on an
// observer that reports RecordKept=false unconditionally, which would page every
// healthy estate and get the rule silenced.
func TestASuccessfullyPublishedRecordIsObservedAsKept(t *testing.T) {
	obs := &capturingObserver{}
	m := &scriptedModel{turns: []llm.Response{{Text: "flat", StopReason: llm.StopEndTurn, Model: "m/1"}}}
	a := agentOver(t, m, &capturingRecorder{}, WithAnswerObserver(obs.fn()))

	if _, err := a.Ask(context.Background(), recPrincipal(), "how is the book?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !obs.only(t).RecordKept {
		t.Fatal("RecordKept = false on a record that published cleanly — every healthy deployment " +
			"would drive kanz_copilot_unrecorded_answers_total and the critical rule would be muted")
	}
}

// THE OBSERVATION CARRIES NO PRINCIPAL AND NO TENANT.
//
// It crosses into a metrics registry, where a tenant label is unbounded
// cardinality AND a disclosure surface — one tenant's question volume readable
// off another's dashboard, which tenant isolation forbids by construction. The
// isolation-bearing detail belongs on the record, which is tenant-scoped.
func TestTheObservationCarriesNoTenantIdentity(t *testing.T) {
	// A compile-time check expressed as a field-set assertion: adding a Tenant or
	// Principal field to Observation must fail here rather than in review.
	var o Observation
	if got := observationFieldNames(o); len(got) == 0 {
		t.Fatal("could not read Observation's fields")
	}
	for _, name := range observationFieldNames(o) {
		switch name {
		case "Tenant", "TenantID", "TenantId", "Principal", "PrincipalSubject", "Subject", "PortfolioID":
			t.Fatalf("Observation carries %q. This struct is consumed by a Prometheus observer, so any "+
				"identity on it becomes a metric label: unbounded cardinality, and one tenant's "+
				"question volume legible from another tenant's dashboard. Tenant isolation is "+
				"deny-by-default and covers discovery, not just reads. Put it on the answer "+
				"record, which is tenant-scoped by construction (#973).", name)
		}
	}
}

// COVERAGE TRAVELS THE WHOLE PATH: read plane → tool result → loop → observation
// AND record.
//
// Each hop is a place it can be dropped, and dropping it anywhere returns the
// estate to the state where an answer composed around missing measures reads
// exactly like one composed on a complete book. The record and the metric are
// asserted TOGETHER because they answer different questions — "how much of the
// book is the agent missing across the estate" and "was it missing from THIS
// answer" — and only the pair makes a CopilotContextWithheld page actionable.
func TestCoverageReachesBothTheObservationAndTheRecord(t *testing.T) {
	var v = 1_250_000.0
	client := governed.NewStubClient()
	client.Put(governed.Reading{
		PortfolioID: "pf-1", Tenant: "acme", Kind: "measures",
		SourceEventID: "evt-1", AsOf: recT0,
		Measures: []measureread.Measure{
			{Name: "VaR99", Status: measureread.StatusMeasured, Value: &v},
			{Name: "DV01", Status: measureread.StatusUnavailable, Reason: "integrity record absent"},
			{Name: "CS01", Status: measureread.StatusUnavailable, Reason: "no marks in the window"},
		},
	})
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}),
		nil, "copilot", slog.New(slog.NewTextHandler(discard{}, nil)))
	reg := tools.NewRegistry(authz, client, retrieval.IdentityCatalog{}, nil)

	m := &scriptedModel{turns: []llm.Response{
		{StopReason: llm.StopToolUse, Model: "m/1", ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "pf-1"},
		}}},
		{Text: "VaR99 is 1250000; DV01 and CS01 are unavailable", StopReason: llm.StopEndTurn, Model: "m/1"},
	}}
	obs := &capturingObserver{}
	rec := &capturingRecorder{}
	a := New(m, reg, WithRecorder(rec), WithClock(func() time.Time { return recT0 }),
		WithAnswerObserver(obs.fn()))

	p := &auth.Principal{Subject: "alice@desk", Tenant: "acme", Roles: []string{"analyst"}}
	if _, err := a.Ask(context.Background(), p, "how is the book?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	got := obs.only(t)
	if got.Measured != 1 || got.Withheld != 2 {
		t.Fatalf("observation coverage = (%d measured, %d withheld), want (1, 2). The read plane "+
			"withheld two of three measures and the agent answered anyway; if that does not reach "+
			"the metric, kanz_copilot_context_measures_total under-reports exactly the gap it "+
			"exists to show (#757/#973).", got.Measured, got.Withheld)
	}
	if r := rec.only(t); r.GetContextMeasured() != 1 || r.GetContextWithheld() != 2 {
		t.Fatalf("record coverage = (%d, %d), want (1, 2). The metric says how much of the book the "+
			"estate is missing; the record is the only thing that says it for the ONE answer an "+
			"operator is disputing.", r.GetContextMeasured(), r.GetContextWithheld())
	}
}
