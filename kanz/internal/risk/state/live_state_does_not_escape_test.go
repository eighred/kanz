package state_test

// EVERY READ OUT OF THIS PACKAGE IS A COPY (#821).
//
// Store.Lookup used to return the engine's live *domain.Portfolio and tell the
// caller to "read it while holding the appropriate lock" — a lock (s.locks[id])
// that is unexported and unreachable from here. These tests pin the property
// that replaced it: the reads that remain hand back a clone, so a caller cannot
// reach the state the store guards, and domain.Portfolio.Positions() — which
// WRITES the receiver's memoised sorted view — can only ever write a value one
// goroutine owns.
//
// WHAT THESE DO NOT PROVE. They are not a race detector. -race needs cgo and
// does not run on the Windows box, so CI is the only detector of the race
// itself. What is provable here, and is what these assert, is the structural
// half: the value handed out is a copy, a mutation through it does not reach
// the store, and the memoising accessor writes only the caller's own copy.
// test/arch/risk_state_hands_out_no_live_portfolio_test.go keeps a fourth read
// from being added without the clone.

import (
	"context"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
)

// seedTwoPositions installs a portfolio holding AAPL and MSFT.
func seedTwoPositions(t *testing.T, s *state.Store) {
	t.Helper()
	ctx := context.Background()
	for i, inst := range []string{"AAPL", "MSFT"} {
		err := s.ApplyPositionChanged(ctx, env("seed-"+inst), &domainpb.PositionState{
			PortfolioId:  "PORT-1",
			InstrumentId: inst,
			Quantity:     &commonpb.Decimal{Coefficient: int64(i + 1)},
			MarketValue:  money(int64(i+1), "USD"),
			AsOf:         offset(0),
		})
		if err != nil {
			t.Fatalf("seed %s: %v", inst, err)
		}
	}
}

// injected is the mutation an outside caller performs on whatever it was handed.
var injected = domain.Position{
	InstrumentID: "INJECTED",
	Quantity:     &commonpb.Decimal{Coefficient: 999},
	MarketValue:  &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 999}},
}

// TestSnapshotMutationDoesNotReachTheStore is the direct replacement for what
// Lookup allowed. Before #821 the equivalent write landed in the engine's live
// book and the next measures query computed over it.
func TestSnapshotMutationDoesNotReachTheStore(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)

	got, ok := s.Snapshot("PORT-1")
	if !ok {
		t.Fatal("Snapshot miss")
	}
	got.SetPosition(injected)
	got.Forget("AAPL")

	fresh, _ := s.Snapshot("PORT-1")
	if _, leaked := fresh.Position("INJECTED"); leaked {
		t.Error("a position added through the Snapshot result reached the store — the read is " +
			"handing out the live portfolio, which is what #821 removed")
	}
	if _, gone := fresh.Position("AAPL"); !gone {
		t.Error("a Forget through the Snapshot result reached the store")
	}
	if n := len(fresh.Positions()); n != 2 {
		t.Errorf("store holds %d positions, want 2 — the caller's edits landed in the engine's book", n)
	}
}

// TestSnapshotWithKeysMutationDoesNotReachTheStore covers the PERS-01c
// snapshotter's read. It returns the (state, dedup-tail) pair, and the state
// half must be a copy for the same reason.
func TestSnapshotWithKeysMutationDoesNotReachTheStore(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)

	got, _, ok := s.SnapshotWithKeys("PORT-1")
	if !ok {
		t.Fatal("SnapshotWithKeys miss")
	}
	got.SetPosition(injected)

	fresh, _ := s.Snapshot("PORT-1")
	if _, leaked := fresh.Position("INJECTED"); leaked {
		t.Error("a mutation through the SnapshotWithKeys result reached the store")
	}
}

// TestSnapshotOwnedMutationDoesNotReachTheStore covers the ownership-aware read
// (#110), which is the one a sharded replica serves from.
func TestSnapshotOwnedMutationDoesNotReachTheStore(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)

	got, err := s.SnapshotOwned("PORT-1")
	if err != nil {
		t.Fatalf("SnapshotOwned: %v", err)
	}
	got.SetPosition(injected)

	fresh, err := s.SnapshotOwned("PORT-1")
	if err != nil {
		t.Fatalf("SnapshotOwned (second): %v", err)
	}
	if _, leaked := fresh.Position("INJECTED"); leaked {
		t.Error("a mutation through the SnapshotOwned result reached the store")
	}
}

// TestEachSnapshotMemoisesItsOwnPositions is the half of #821 that is about the
// accessor rather than the pointer. Positions() WRITES the receiver's sortedCache
// on the first call after a mutation. This asserts that write lands on the
// caller's own clone: two snapshots of the same portfolio must not share the
// slice, or one caller's Positions() would be publishing into another's — and,
// before #821, into the live book the ingest goroutine is applying to.
func TestEachSnapshotMemoisesItsOwnPositions(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)

	a, _ := s.Snapshot("PORT-1")
	b, _ := s.Snapshot("PORT-1")

	pa := a.Positions() // performs the memoising write on a
	pb := b.Positions() // and on b

	if len(pa) != 2 || len(pb) != 2 {
		t.Fatalf("positions a=%d b=%d, want 2 and 2", len(pa), len(pb))
	}
	if &pa[0] == &pb[0] {
		t.Fatal("two snapshots returned the SAME backing array from Positions() — the memoised " +
			"view is shared, so one reader's accessor writes another's state")
	}

	// The same call twice on ONE snapshot must still be memoised: that is the
	// 5.7x the cache is there for (see domain.Portfolio.sortedCache), and a
	// change that made Positions() pure would silently take it away.
	if again := a.Positions(); &again[0] != &pa[0] {
		t.Error("Positions() rebuilt the view on a second call — the memoisation this trade-off " +
			"was measured for has regressed; see compute/regression_test.go")
	}
}

// TestApplyAfterSnapshotDoesNotChangeTheSnapshot pins the other direction: the
// clone is a point-in-time view, so an apply landing afterwards must not appear
// in a slice a caller is mid-computation over.
func TestApplyAfterSnapshotDoesNotChangeTheSnapshot(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)

	got, _ := s.Snapshot("PORT-1")
	before := len(got.Positions())

	err := s.ApplyPositionChanged(context.Background(), env("later"), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "TSLA",
		Quantity:     &commonpb.Decimal{Coefficient: 3},
		MarketValue:  money(3, "USD"),
		AsOf:         offset(1),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if after := len(got.Positions()); after != before {
		t.Errorf("the snapshot's position count moved from %d to %d under a later apply — the "+
			"caller is computing against the live book, not a point-in-time view", before, after)
	}
	if fresh, _ := s.Snapshot("PORT-1"); len(fresh.Positions()) != before+1 {
		t.Error("the later apply did not reach the store — the seed for this test is wrong")
	}
}
