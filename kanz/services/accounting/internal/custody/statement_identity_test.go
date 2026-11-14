package custody

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"
)

type statementSubjectChange struct {
	name   string
	change func(*Statement)
}

func statementSubjectChanges() []statementSubjectChange {
	return []statementSubjectChange{
		{"portfolio", func(s *Statement) { s.PortfolioID += "-other" }},
		{"custodian", func(s *Statement) { s.CustodianID += "-other" }},
		{"business_date", func(s *Statement) { s.BusinessDate = s.BusinessDate.AddDate(0, 0, 1) }},
	}
}

func TestStatementIdentityCannotMoveAcrossSubjects(t *testing.T) {
	for _, change := range statementSubjectChanges() {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", change.name, reverse), func(t *testing.T) {
				assertStatementIdentityRefusal(t, NewMemoryStore(), change, reverse)
			})
		}
	}
}

// The same contract exercises both stores. Only the subject changes; valid
// economics/receipt fields ensure the identity check causes the refusal.
func assertStatementIdentityRefusal(t *testing.T, store Store, change statementSubjectChange, reverse bool) {
	t.Helper()
	original := statement("identity-"+t.Name(), map[string]int64{"AAPL": 90}, nil)
	original.PortfolioID = t.Name()
	conflicting := cloneStatement(original)
	change.change(&conflicting)
	if reverse {
		original, conflicting = conflicting, original
	}
	conflicting.Positions["AAPL"] = big.NewRat(100, 1)
	conflicting.ReceivedAt = original.ReceivedAt.Add(time.Hour)
	for range 2 {
		if err := store.SaveStatement(t.Context(), original); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.LatestStatement(t.Context(), original.Subject())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.SaveStatement(t.Context(), conflicting); !errors.Is(err, ErrStatementIdentityConflict) {
			t.Fatalf("source identity moved to another %s: err=%v", change.name, err)
		}
		got, err := store.LatestStatement(t.Context(), original.Subject())
		if err != nil || !reflect.DeepEqual(got, before) {
			t.Fatalf("refusal rewrote original subject/economics: before=%+v after=%+v err=%v", before, got, err)
		}
		if got, err := store.LatestStatement(t.Context(), conflicting.Subject()); !errors.Is(err, ErrNoStatement) || got.StatementID != "" {
			t.Fatalf("refusal created evidence under another subject: %+v %v", got, err)
		}
	}
	conflicting.StatementID += "-independent"
	if err := store.SaveStatement(t.Context(), conflicting); err != nil {
		t.Fatal(err)
	}
	if got, err := store.LatestStatement(t.Context(), conflicting.Subject()); err != nil || got.StatementID != conflicting.StatementID || got.Positions["AAPL"].Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("independent evidence could not recover the target subject: %+v %v", got, err)
	}
}

func TestStatementIdentityPreservesNormalizedSubjectRestatements(t *testing.T) {
	assertStatementIdentityRestatement(t, NewMemoryStore())
}

func assertStatementIdentityRestatement(t *testing.T, store Store) {
	t.Helper()
	original := statement("restatement", map[string]int64{"AAPL": 90}, nil)
	original.PortfolioID = "RESTATEMENT"
	if err := store.SaveStatement(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	updated := cloneStatement(original)
	// Sep 2 at 01:00 +14 is Sep 1 at 11:00 UTC, the original UTC business date.
	updated.BusinessDate = time.Date(2026, 9, 2, 1, 0, 0, 0, time.FixedZone("plus14", 14*3600))
	updated.ReceivedAt = t0.Add(time.Hour)
	updated.Positions["AAPL"] = big.NewRat(100, 1)
	for range 2 {
		if err := store.SaveStatement(t.Context(), updated); err != nil {
			t.Fatalf("same normalized subject restatement refused: %v", err)
		}
	}
	got, err := store.LatestStatement(t.Context(), original.Subject())
	if err != nil || got.Subject().Key() != original.Subject().Key() || got.Positions["AAPL"].Cmp(big.NewRat(100, 1)) != 0 || !got.ReceivedAt.Equal(updated.ReceivedAt) {
		t.Fatalf("same-subject policy changed: %+v %v", got, err)
	}
}

func TestStatementIdentityRefusalKeepsRunHistoryAndLifecycle(t *testing.T) {
	ctx := testRunContext(t.Context())
	store := NewMemoryStore()
	original := statement("investigated-statement", map[string]int64{"AAPL": 90}, nil)
	if err := store.SaveStatement(ctx, original); err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	r := newReconciler(t, store, bookWith(map[string]int64{"AAPL": 100}, nil), pub, nil, func() time.Time { return t0 })
	first, err := r.Reconcile(ctx, subject())
	if err != nil || first.Outcome != OutcomeBreaks || len(first.Breaks) != 1 {
		t.Fatalf("fixture did not establish original run: %+v %v", first, err)
	}
	before := investigatedReferenceBreaks(t, ctx, store)
	for _, change := range statementSubjectChanges() {
		conflicting := cloneStatement(original)
		change.change(&conflicting)
		if err := store.SaveStatement(ctx, conflicting); !errors.Is(err, ErrStatementIdentityConflict) {
			t.Fatalf("relabeled source was accepted: %s %v", change.name, err)
		}
		assertReferenceLifecycle(t, ctx, store, before)
		latest, err := store.LatestRun(ctx, "PF1", "CUST-A")
		if err != nil || !reflect.DeepEqual(latest, first) || len(pub.events) != 1 || store.outbox.PendingCount() != 0 || len(store.runIDs) != 1 {
			t.Fatalf("rejected ingress changed completed history/outbox: %+v %v", latest, err)
		}
	}
}
