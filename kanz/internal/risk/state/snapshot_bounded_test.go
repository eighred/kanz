package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/risk/state"
)

func TestSnapshotOwnedBoundedPreservesOwnershipAndCompleteClone(t *testing.T) {
	s := state.NewStore()
	seedTwoPositions(t, s)
	for _, limit := range []int{0, -1} {
		if _, err := s.SnapshotOwnedBounded("PORT-1", limit); !errors.Is(err, state.ErrInvalidSnapshotLimit) {
			t.Fatalf("invalid limit accepted: %v", err)
		}
	}
	if got, err := s.SnapshotOwnedBounded("PORT-1", 1); got != nil || !errors.Is(err, state.ErrSnapshotPositionLimit) {
		t.Fatalf("oversized snapshot returned a partial book: %v %v", got, err)
	}
	got, err := s.SnapshotOwnedBounded("PORT-1", 2)
	if err != nil || len(got.Positions()) != 2 {
		t.Fatalf("complete boundary snapshot: %v %v", got, err)
	}
	got.Forget("AAPL")
	fresh, err := s.SnapshotOwned("PORT-1")
	if err != nil || len(fresh.Positions()) != 2 {
		t.Fatalf("bounded clone escaped or legacy behavior changed: %v %v", fresh, err)
	}
	loader := newFakeLoader()
	loader.recs["OWN"] = durableRecord("OWN")
	owned := state.NewStore(state.WithRuntimeOwnership(loader))
	if _, err := owned.SnapshotOwnedBounded("OWN", 1); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("unacquired bounded snapshot answered: %v", err)
	}
	if err := owned.Acquire(context.Background(), "OWN"); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.SnapshotOwnedBounded("OWN", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owned.Release("OWN"); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.SnapshotOwnedBounded("OWN", 1); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("released bounded snapshot answered: %v", err)
	}
}
