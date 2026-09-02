package tools

// Tool-result coverage tests (#973).
//
// The tool result is where the read plane's verdict crosses into the agent. It
// already carried the values the model may cite; what it did not carry was how
// much it REFUSED to state, so an agent shown one measure out of four looked
// exactly like one shown a complete book.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/measureread"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

func coverageRegistry(t *testing.T, r governed.Reading) *Registry {
	t.Helper()
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{
			"analyst": {auth.ActionRiskRead},
		}}), nil, "copilot", slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	client := governed.NewStubClient()
	client.Put(r)
	return NewRegistry(authz, client, retrieval.IdentityCatalog{}, nil)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func coverageReading() governed.Reading {
	var var99 = 1_250_000.0
	return governed.Reading{
		PortfolioID: "pf-1", Tenant: "acme", Kind: "measures",
		SourceEventID: "evt-1", AsOf: time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC),
		Measures: []measureread.Measure{
			{Name: "VaR99", Status: measureread.StatusMeasured, Value: &var99},
			{Name: "DV01", Status: measureread.StatusUnavailable, Reason: "integrity record absent"},
			{Name: "CS01", Status: measureread.StatusUnavailable, Reason: "no marks in the window"},
		},
	}
}

// THE RESULT REPORTS WHAT WAS WITHHELD, NOT ONLY WHAT WAS STATED.
//
// Values is MEASURED-only by design — admitting a withheld number there would
// license the model to state exactly what this plane refused to state. That is
// correct and it is also why the counts have to travel separately: without them
// the agent can see three measures came back and two are missing, and nothing
// downstream can.
func TestAToolResultReportsWithheldMeasures(t *testing.T) {
	r := coverageRegistry(t, coverageReading())
	p := &auth.Principal{Subject: "alice@desk", Tenant: "acme", Roles: []string{"analyst"}}

	out := r.Invoke(context.Background(), p, llm.ToolCall{
		Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "pf-1"},
	})
	if out.IsError {
		t.Fatalf("tool errored: %s", out.Content)
	}
	if out.Measured != 1 || out.Unavailable != 2 {
		t.Fatalf("result coverage = (%d measured, %d unavailable), want (1, 2). The model was shown "+
			"three measures and may state one; a result that does not carry that split makes an "+
			"answer composed around a gap indistinguishable from one composed on a complete "+
			"book (#757/#973).", out.Measured, out.Unavailable)
	}
	if len(out.Values) != out.Measured {
		t.Fatalf("Values has %d entries and Measured is %d — these are one judgement read twice, and "+
			"when they disagree the coverage count describes a reading the model did not get",
			len(out.Values), out.Measured)
	}
}

// AN ERRORED RESULT CARRIES NO COVERAGE. A denied read must not look like a
// withheld measure: it would inflate the withheld share and page
// CopilotContextWithheld on what is actually an access problem.
func TestADeniedReadReportsNoCoverage(t *testing.T) {
	r := coverageRegistry(t, coverageReading())
	// A principal of another tenant: the gate denies, and the refusal text is a
	// closed set that names nothing about the resource's owner.
	p := &auth.Principal{Subject: "mallory@other", Tenant: "other", Roles: []string{"analyst"}}

	out := r.Invoke(context.Background(), p, llm.ToolCall{
		Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "pf-1"},
	})
	if !out.IsError {
		t.Fatal("a cross-tenant read was allowed — this test's premise is gone and tenant isolation is broken")
	}
	if out.Measured != 0 || out.Unavailable != 0 {
		t.Fatalf("a denied read reported coverage (%d, %d), want (0, 0). An authorization deny is not "+
			"a data-quality gap, and counting it as one drives CopilotContextWithheld on an access "+
			"problem.", out.Measured, out.Unavailable)
	}
}
