package lineage

import (
	"context"
	"testing"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
)

// seed appends a small correlated cascade:
//
//	tick (root) -> signal -> order -> outcome   (a causal chain)
//	tick        -> alert                        (a sibling branch)
func seed(t *testing.T) *audit.Memory {
	t.Helper()
	st := audit.NewMemory()
	ctx := context.Background()
	recs := []*audit.Record{
		{EventID: "tick", CorrelationID: "C", CausationID: "", Kind: audit.KindEvent},
		{EventID: "signal", CorrelationID: "C", CausationID: "tick", Kind: audit.KindDecision},
		{EventID: "order", CorrelationID: "C", CausationID: "signal", Kind: audit.KindCommand},
		{EventID: "outcome", CorrelationID: "C", CausationID: "order", Kind: audit.KindCommandOutcome},
		{EventID: "alert", CorrelationID: "C", CausationID: "tick", Kind: audit.KindDataQuality},
	}
	for _, r := range recs {
		if _, err := st.Append(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestReconstructAncestry(t *testing.T) {
	st := seed(t)
	lin, err := Reconstruct(context.Background(), st, "outcome")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tick", "signal", "order", "outcome"}
	if len(lin.Ancestry) != len(want) {
		t.Fatalf("ancestry len=%d want %d", len(lin.Ancestry), len(want))
	}
	for i, id := range want {
		if lin.Ancestry[i].EventID != id {
			t.Fatalf("ancestry[%d]=%s want %s (must be root-first)", i, lin.Ancestry[i].EventID, id)
		}
	}
}

func TestReconstructTree(t *testing.T) {
	st := seed(t)
	lin, err := Reconstruct(context.Background(), st, "order")
	if err != nil {
		t.Fatal(err)
	}
	if lin.Tree == nil || lin.Tree.Record.EventID != "tick" {
		t.Fatalf("tree root = %v want tick", lin.Tree)
	}
	// tick has two children: signal and alert.
	if len(lin.Tree.Children) != 2 {
		t.Fatalf("root children=%d want 2", len(lin.Tree.Children))
	}
}

func TestReconstructNotFound(t *testing.T) {
	st := seed(t)
	if _, err := Reconstruct(context.Background(), st, "nope"); err != ErrNotFound {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}
