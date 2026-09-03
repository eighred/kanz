package state_test

// The release observer (#893).
//
// state.Store.Release prunes this package's four maps. Everything DERIVED from
// that state lives a layer up — risk.Cache holds a released portfolio's
// last-known-good exposure and measure sets — and nothing told it, so a replica
// on a rebalancing ring accumulated every portfolio it had ever owned.
//
// The observer is what keeps the two prunings in one call path. These tests are
// about the seam's contract; the cache's own behaviour is covered in
// internal/risk, and the two are joined end-to-end against real Postgres in
// ownership_postgres_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// emptyLoader is a durable store holding nothing: every Acquire takes ownership
// with no in-memory entry, which is the legal "never snapshotted" case and is
// all these tests need.
type emptyLoader struct{}

func (emptyLoader) Load(context.Context, v1.PortfolioID) (persist.PortfolioRecord, error) {
	return persist.PortfolioRecord{}, persist.ErrNotFound
}

// recorder captures the ids the store released.
type recorder struct {
	mu  sync.Mutex
	ids []v1.PortfolioID
}

func (r *recorder) observe(id v1.PortfolioID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *recorder) seen() []v1.PortfolioID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1.PortfolioID(nil), r.ids...)
}

// A RELEASE NOTIFIES THE OBSERVER WITH THE ID IT DROPPED.
func TestReleaseNotifiesTheObserver(t *testing.T) {
	rec := &recorder{}
	s := state.NewStore(
		state.WithRuntimeOwnership(emptyLoader{}),
		state.WithReleaseObserver(rec.observe),
	)
	const id v1.PortfolioID = "PORT-REL"
	if err := s.Acquire(context.Background(), id); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if _, released, err := s.Release(id); err != nil || !released {
		t.Fatalf("Release = (%v, %v), want (nil, true)", err, released)
	}
	got := rec.seen()
	if len(got) != 1 || got[0] != id {
		t.Fatalf("observer saw %v, want exactly [%q]. Everything derived from a released "+
			"portfolio's state is pruned through this call; a release that does not fire it "+
			"leaves the derived copy resident for the life of the process (#893).", got, id)
	}
}

// A PORTFOLIO THIS REPLICA DOES NOT HOLD DOES NOT FIRE IT.
//
// Release returns released=false for an unowned portfolio because a watcher
// re-delivering a revocation is normal. Firing the observer there would make a
// duplicate revocation evict a portfolio a DIFFERENT replica now owns, if the
// two ever shared a cache — and it would make the observer's call count
// meaningless as a signal of actual handoffs.
func TestReleasingAnUnownedPortfolioDoesNotNotify(t *testing.T) {
	rec := &recorder{}
	s := state.NewStore(
		state.WithRuntimeOwnership(emptyLoader{}),
		state.WithReleaseObserver(rec.observe),
	)

	_, released, err := s.Release("never-acquired")
	if err != nil || released {
		t.Fatalf("Release of an unowned portfolio = (%v, %v), want (nil, false)", err, released)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("observer fired for a portfolio that was never owned: %v", got)
	}
}

// A STRUCTURALLY INVALID RELEASE DOES NOT FIRE IT EITHER. An empty id is an
// error, not a handoff.
func TestAnInvalidReleaseDoesNotNotify(t *testing.T) {
	rec := &recorder{}
	s := state.NewStore(
		state.WithRuntimeOwnership(emptyLoader{}),
		state.WithReleaseObserver(rec.observe),
	)

	if _, _, err := s.Release(""); !errors.Is(err, state.ErrEmptyPortfolioID) {
		t.Fatalf("Release(\"\") = %v, want ErrEmptyPortfolioID", err)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("observer fired on an invalid release: %v", got)
	}
}

// NO OBSERVER IS NOT A PANIC. Nil is the pre-#893 behaviour and remains legal
// for a store whose caller derives nothing from it — every test store in this
// package, for one.
func TestReleaseWithNoObserverIsSafe(t *testing.T) {
	s := state.NewStore(state.WithRuntimeOwnership(emptyLoader{}))
	const id v1.PortfolioID = "PORT-NOOBS"
	if err := s.Acquire(context.Background(), id); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, released, err := s.Release(id); err != nil || !released {
		t.Fatalf("Release = (%v, %v), want (nil, true)", err, released)
	}
}
