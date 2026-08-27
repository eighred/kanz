package tools

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/measureread"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

// A MEASURE THE ENGINE COULD NOT VOUCH FOR MUST NOT REACH THE MODEL AS A NUMBER
// (#757).
//
// The fixed-income family is registered in production over a contract-terms
// store with no production writer, so DV01 arrives as a zero computed over zero
// bonds. Rendered as "DV01 = 0", a model answers "your book carries no
// interest-rate risk" — byte-identical to the truth for a book holding no bonds.
func withheldHarness(t *testing.T) *Registry {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(discard{}, nil))
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(testPolicy()), &capRecorder{}, "copilot", quiet)
	stub := governed.NewStubClient()
	sound := 1250000.0
	stub.Put(governed.Reading{
		PortfolioID: "PF-T1",
		Tenant:      "t1",
		Kind:        "measures",
		Measures: []measureread.Measure{
			{Name: "DV01", Status: measureread.StatusUnavailable,
				Reason:   "computed over 0 position(s) with 12 excluded (no_terms)",
				Coverage: measureread.Coverage{Reported: true, Excluded: 12}},
			{Name: "VaR99", Status: measureread.StatusMeasured, Value: &sound},
		},
		QualityFlags:  []string{"INPUTS_UNRESOLVED"},
		SourceEventID: "evt-123",
		AsOf:          time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	})
	return NewRegistry(authz, stub, retrieval.IdentityCatalog{}, quiet)
}

func TestInvoke_AWithheldMeasureIsRenderedAsARefusalNotANumber(t *testing.T) {
	out := withheldHarness(t).Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})
	if out.IsError {
		t.Fatalf("expected success, got error: %s", out.Content)
	}

	if strings.Contains(out.Content, "DV01 = 0") {
		t.Fatalf("the model was handed DV01 = 0 for a measure computed over nothing:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "DV01") || !strings.Contains(out.Content, "UNAVAILABLE") {
		t.Errorf("DV01 is not named as unavailable — silently dropping the line reads as \"the book "+
			"has no such risk\", which is the same false claim:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "no_terms") {
		t.Errorf("the refusal does not say WHY — an operator cannot act on it, and cannot learn "+
			"which store is empty:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "INPUTS_UNRESOLVED") {
		t.Errorf("the response's quality flags never reach the model:\n%s", out.Content)
	}
	// NON-VACUITY: the sound measure beside the withheld one is still served.
	// Refusing both is the all-or-nothing #509 removed.
	if !strings.Contains(out.Content, "VaR99") || strings.Contains(out.Content, "VaR99: UNAVAILABLE") {
		t.Errorf("the sound measure was withheld alongside the degraded one:\n%s", out.Content)
	}
}

// THE GROUNDING CONSEQUENCE, WHICH IS THE HALF THAT BITES.
//
// Result.Values is what agent.go accumulates into citedValues, and
// retrieval.CheckGrounding treats that slice as "numbers a tool actually
// returned". A withheld value admitted here would license the model to state
// the exact number this plane refused to state, and the anti-hallucination gate
// would wave it through.
func TestInvoke_AWithheldMeasureIsNotCitableForGrounding(t *testing.T) {
	out := withheldHarness(t).Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if len(out.Values) != 1 {
		t.Fatalf("values = %v, want exactly the one MEASURED value — a withheld measure in this "+
			"slice is a licence for the model to state it", out.Values)
	}
	if out.Values[0] != 1250000 {
		t.Errorf("values[0] = %v, want the sound VaR99", out.Values[0])
	}
	for _, v := range out.Values {
		if v == 0 {
			t.Errorf("a zero reached citedValues — that is the confident zero re-entering through "+
				"the grounding gate: %v", out.Values)
		}
	}
}
