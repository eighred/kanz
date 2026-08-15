package projector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

var now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func fixedClock() Option { return WithClock(func() time.Time { return now }) }

// two vendors mastering INST1, disagreeing on its ISIN (an identifier conflict)
// and on its price (a tolerance breach is not reachable with two candidates, so
// the price here is only used for the missing/stale paths).
func vendors() []feed.VendorFeed {
	return []feed.VendorFeed{
		feed.SimFeed{
			Name: "BLOOMBERG",
			VRecords: []master.VendorRecord{{
				Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0,
				AssetClass: "EQUITY", CurrencyCode: "USD",
				Identifiers: master.Identifiers{ISIN: "US0000001"}, AsOf: now,
			}},
			Candidates: []pricing.Candidate{{InstrumentID: "INST1", Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now}},
		},
		feed.SimFeed{
			Name: "ICE",
			VRecords: []master.VendorRecord{{
				Vendor: "ICE", InstrumentID: "INST1", Priority: 1,
				Description: "Apple Inc",
				Identifiers: master.Identifiers{ISIN: "US9999999"}, AsOf: now, // conflicting ISIN
			}},
			Candidates: []pricing.Candidate{{InstrumentID: "INST1", Source: "ICE", Price: dec.Rat("101"), AsOf: now}},
		},
	}
}

func newProjector(feeds []feed.VendorFeed) (*Projector, *store.MemoryGoldenStore, store.ExceptionStore) {
	golden := store.NewMemoryGoldenStore()
	exceptions := store.NewQueueStore(pricing.NewQueue())
	return New(feeds, golden, exceptions, nil, fixedClock()), golden, exceptions
}

// The golden store had no writer at all. This is it: a refresh resolves the
// vendors into one record per instrument and persists it.
func TestRefreshPersistsTheGoldenRecord(t *testing.T) {
	p, golden, _ := newProjector(vendors())
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := golden.Get(context.Background(), "INST1")
	if err != nil || !ok {
		t.Fatalf("golden record not persisted: ok=%v err=%v", ok, err)
	}
	// Survivorship: the higher-priority vendor wins a contested field, the lower
	// one fills what the winner left empty.
	if rec.CurrencyCode != "USD" {
		t.Errorf("currency = %q, want USD (BLOOMBERG, priority 0)", rec.CurrencyCode)
	}
	if rec.Description != "Apple Inc" {
		t.Errorf("description = %q, want Apple Inc (ICE filled what BLOOMBERG left empty)", rec.Description)
	}
}

// A vendor disagreement on an identifier is a break a human must adjudicate — it
// is filed on the cycle, not when somebody happens to read the instrument.
func TestRefreshFilesIdentifierConflicts(t *testing.T) {
	p, _, exceptions := newProjector(vendors())
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, err := exceptions.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range open {
		if e.Kind == pricing.KindIdentifierConflict && e.InstrumentID == "INST1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ISIN conflict was not filed: %v", open)
	}
}

// An instrument in the master with no quote anywhere is a MISSING_PRICE break —
// surfaced by the cycle, without waiting for a human to read the price endpoint.
func TestRefreshFilesMissingPrice(t *testing.T) {
	feeds := []feed.VendorFeed{feed.SimFeed{
		Name: "BLOOMBERG",
		VRecords: []master.VendorRecord{{
			Vendor: "BLOOMBERG", InstrumentID: "NOPRICE", Priority: 0, CurrencyCode: "USD", AsOf: now,
		}},
	}}
	p, _, exceptions := newProjector(feeds)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, err := exceptions.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != pricing.KindMissingPrice || open[0].InstrumentID != "NOPRICE" {
		t.Fatalf("want one MISSING_PRICE for NOPRICE, got %v", open)
	}
}

// failing is a vendor that is down.
type failing struct{}

func (failing) Vendor() string { return "DOWN" }
func (failing) Records(context.Context) ([]master.VendorRecord, error) {
	return nil, errors.New("vendor unreachable")
}
func (failing) Prices(context.Context) ([]pricing.Candidate, error) {
	return nil, errors.New("vendor unreachable")
}

// TestRefreshIsAllOrNothingWhenAVendorIsDown pins the degrade rule.
//
// Survivorship resolves a field from the highest-priority vendor that HAS it, so
// projecting while a vendor is unreachable does not produce a partial record — it
// produces a DIFFERENT one, silently sourced from a fallback vendor and stamped
// with provenance as though that were the intended answer. The cycle aborts and
// the store keeps the last good projection. Stale and correct beats fresh and
// quietly degraded.
func TestRefreshIsAllOrNothingWhenAVendorIsDown(t *testing.T) {
	ctx := context.Background()
	p, golden, _ := newProjector(vendors())
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before, _, _ := golden.Get(ctx, "INST1")

	// BLOOMBERG (the priority-0 vendor) goes down; ICE alone would still resolve a
	// complete-looking record for INST1, with ICE's conflicting ISIN surviving.
	degraded := New([]feed.VendorFeed{failing{}, vendors()[1]}, golden, store.NewQueueStore(nil), nil, fixedClock())
	if err := degraded.Refresh(ctx); err == nil {
		t.Fatal("a refresh with an unreachable vendor must fail, not master a book from the vendors that answered")
	}

	after, ok, _ := golden.Get(ctx, "INST1")
	if !ok {
		t.Fatal("the failed refresh destroyed the previous projection")
	}
	if after.Identifiers.ISIN != before.Identifiers.ISIN {
		t.Fatalf("the failed refresh overwrote the golden ISIN: %q → %q", before.Identifiers.ISIN, after.Identifiers.ISIN)
	}
}

// A break that a human has already adjudicated must not be re-opened by the next
// cycle re-detecting it. The exception id is deterministic and the store's add is
// idempotent — this pins that the projector actually relies on both.
func TestRefreshDoesNotReopenAnAdjudicatedBreak(t *testing.T) {
	ctx := context.Background()
	p, _, exceptions := newProjector(vendors())
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	open, err := exceptions.Open(ctx)
	if err != nil || len(open) == 0 {
		t.Fatalf("no exceptions filed: %v %v", open, err)
	}
	id := open[0].ID
	if err := exceptions.Override(ctx, id, pricing.Override{Actor: "alice@kanz", Reason: "vendor confirmed", ChosenPrice: dec.Rat("101"), At: now}); err != nil {
		t.Fatal(err)
	}

	if err := p.Refresh(ctx); err != nil { // the same break is detected again
		t.Fatal(err)
	}
	ex, ok, err := exceptions.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("exception vanished: ok=%v err=%v", ok, err)
	}
	if ex.Status != pricing.StatusOverridden {
		t.Fatalf("status = %q: the next cycle re-opened a break a human had already signed off", ex.Status)
	}
	if len(ex.Overrides) != 1 {
		t.Fatalf("the override audit trail was disturbed by re-detection: %+v", ex.Overrides)
	}
}

// --- cycle election ------------------------------------------------------

// fakeLock records what the projector asked of it.
type fakeLock struct {
	acquired bool
	err      error
	released bool
	calls    int
}

func (f *fakeLock) TryAcquire(context.Context) (func(), bool, error) {
	f.calls++
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.acquired {
		return nil, false, nil
	}
	return func() { f.released = true }, true, nil
}

// A replica that loses the election does NO work — it does not call the vendors
// and it does not write. That is the whole point: every pod runs the projector, and
// vendor reference data is metered per call, so N replicas must not make N rounds
// of API requests for one cycle's worth of information.
func TestRefreshSkipsTheCycleWhenAnotherReplicaHoldsIt(t *testing.T) {
	ctx := context.Background()
	golden := store.NewMemoryGoldenStore()
	exceptions := store.NewQueueStore(nil)
	lock := &fakeLock{acquired: false}

	// A feed that fails if it is so much as touched: losing the election must not
	// cost a vendor call.
	p := New([]feed.VendorFeed{failing{}}, golden, exceptions, nil, fixedClock(), WithCycleLock(lock))

	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("losing the election is not a failure: %v", err)
	}
	if lock.calls != 1 {
		t.Fatalf("the lock was consulted %d times, want 1", lock.calls)
	}
	if _, ok, _ := golden.Get(ctx, "INST1"); ok {
		t.Fatal("a replica that lost the election still wrote to the golden store")
	}
}

// The winner does the work and always hands the lock back, so the next cycle is
// not starved by this one.
func TestRefreshReleasesTheCycleLock(t *testing.T) {
	lock := &fakeLock{acquired: true}
	p, golden, _ := newProjector(vendors())
	WithCycleLock(lock)(p)

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !lock.released {
		t.Fatal("the winner did not release the cycle lock — no replica could ever project again")
	}
	if _, ok, _ := golden.Get(context.Background(), "INST1"); !ok {
		t.Fatal("the winner did not project")
	}
}

// A lock that cannot be reached is not permission to project: two replicas both
// assuming they won is exactly the duplicate-vendor-call storm the lock prevents.
func TestRefreshFailsWhenTheCycleLockIsUnreachable(t *testing.T) {
	p, _, _ := newProjector(vendors())
	WithCycleLock(&fakeLock{err: errors.New("db down")})(p)

	if err := p.Refresh(context.Background()); err == nil {
		t.Fatal("an unreachable cycle lock must fail the refresh, not proceed as if elected")
	}
}
