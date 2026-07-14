package retrieval

import (
	"context"
	"testing"
)

func TestCheckGrounding(t *testing.T) {
	cited := []float64{1250000, 0.42}

	// An answer that only asserts cited values is grounded (0.42 and a % render).
	g := CheckGrounding("VaR99 is 1250000 and Delta is 0.42 (42%).", cited)
	if !g.Grounded {
		t.Errorf("expected grounded, got ungrounded %v", g.Ungrounded)
	}

	// A fabricated measure is flagged.
	g = CheckGrounding("VaR99 is 9999999.", cited)
	if g.Grounded || len(g.Ungrounded) == 0 {
		t.Errorf("expected hallucinated 9999999 flagged, got %+v", g)
	}

	// Years and small counts are not treated as measures.
	g = CheckGrounding("In 2026 the top 3 exposures dominate; Delta 0.42.", cited)
	if !g.Grounded {
		t.Errorf("year/count should not be flagged, got %v", g.Ungrounded)
	}
}

func TestIdentityCatalog(t *testing.T) {
	cat := IdentityCatalog{}
	node, ok := cat.Resolve(context.Background(), "evt-1")
	if !ok || node != "evt-1" {
		t.Errorf("identity resolve = %q,%v", node, ok)
	}
	if _, ok := cat.Resolve(context.Background(), ""); ok {
		t.Errorf("empty event id should not resolve")
	}
}

func TestCitationString(t *testing.T) {
	c := Citation{SourceEventID: "evt-1", PortfolioID: "PF-1", AsOf: "2026-06-29T00:00:00Z", LineageNode: "ds:risk/PF-1"}
	if got := c.String(); got == "" || got[0] != '[' {
		t.Errorf("citation string = %q", got)
	}
}
