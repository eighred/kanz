package state_test

// Runtime acquire/release (#110). These are the DB-free properties; the
// same acquire against a real Postgres record (RLS on, NOSUPERUSER role) is
// ownership_postgres_test.go, gated on TEST_POSTGRES_URL.
//
// UNPROVEN HERE: -race needs cgo and does not run on the usual dev box, so the
// concurrent-acquire test below proves the OUTCOME (one winner, one install,
// live state not clobbered) and not the absence of a data race. That claim
// rests on CI.

import (
	"context"
	"errors"
	"sync"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// fakeLoader is the durable side of an acquire, counted so the idempotency
// test can prove a second Acquire does not re-read.
type fakeLoader struct {
	mu    sync.Mutex
	recs  map[v1.PortfolioID]persist.PortfolioRecord
	err   error
	calls int
}

func newFakeLoader() *fakeLoader {
	return &fakeLoader{recs: map[v1.PortfolioID]persist.PortfolioRecord{}}
}

func (f *fakeLoader) Load(_ context.Context, id v1.PortfolioID) (persist.PortfolioRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return persist.PortfolioRecord{}, f.err
	}
	rec, ok := f.recs[id]
	if !ok {
		return persist.PortfolioRecord{}, persist.ErrNotFound
	}
	return rec, nil
}

func (f *fakeLoader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// durableRecord is a portfolio as Postgres would hand it back: aggregate
// fields, one position, and an applied-key tail.
func durableRecord(id v1.PortfolioID) persist.PortfolioRecord {
	return persist.PortfolioRecord{
		ID:               id,
		DisplayName:      "Durable Alpha",
		BaseCurrency:     "USD",
		CashBalance:      money(500, "USD"),
		TotalMarketValue: money(9000, "USD"),
		PositionCount:    1,
		AsOf:             baseTime,
		Positions: []domain.Position{{
			InstrumentID: "ACME",
			Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			MarketValue:  money(9000, "USD"),
			AsOf:         baseTime,
		}},
		AppliedKeys: []string{"already-folded"},
	}
}

func revalued(id string) *domainpb.PortfolioState {
	return &domainpb.PortfolioState{
		PortfolioId:      id,
		DisplayName:      "Live",
		BaseCurrency:     "USD",
		CashBalance:      money(1, "USD"),
		TotalMarketValue: money(2, "USD"),
		PositionCount:    7,
		AsOf:             offset(0),
	}
}

// --- the default posture is untouched -------------------------------------

func TestUngatedStoreOwnsEverythingAndRefusesOwnershipCalls(t *testing.T) {
	s := state.NewStore()
	if s.OwnershipManaged() {
		t.Fatal("NewStore() must not gate on ownership — that is the deployed unsharded posture")
	}
	if !s.Owns("ANY-PORTFOLIO") {
		t.Error("ungated store must own every portfolio")
	}
	if err := s.ApplyPortfolioRevalued(context.Background(), env("e1"), revalued("PORT-U")); err != nil {
		t.Fatalf("ungated apply: %v", err)
	}
	if _, ok := s.Lookup("PORT-U"); !ok {
		t.Error("ungated store must still lazy-create on first reference")
	}
	// Acquire/Release on an ungated store are a mechanism error, not a no-op:
	// a caller that believes it released a portfolio the store still answers
	// for is the exact silent-wrong-answer shape this work prevents.
	if err := s.Acquire(context.Background(), "PORT-U"); !errors.Is(err, state.ErrOwnershipUnmanaged) {
		t.Errorf("Acquire on ungated store = %v, want ErrOwnershipUnmanaged", err)
	}
	if _, _, err := s.Release("PORT-U"); !errors.Is(err, state.ErrOwnershipUnmanaged) {
		t.Errorf("Release on ungated store = %v, want ErrOwnershipUnmanaged", err)
	}
	// A nil loader must not silently create a store that refuses everything.
	if state.NewStore(state.WithRuntimeOwnership(nil)).OwnershipManaged() {
		t.Error("WithRuntimeOwnership(nil) must leave the store ungated")
	}
}

// --- an unacquired portfolio is not answerable ----------------------------

func TestGatedStoreRefusesUnacquiredPortfolioRatherThanServingZero(t *testing.T) {
	s := state.NewStore(state.WithRuntimeOwnership(newFakeLoader()))
	if !s.OwnershipManaged() {
		t.Fatal("WithRuntimeOwnership must gate the store")
	}
	if s.Owns("PORT-X") {
		t.Error("a gated store must not own a portfolio it never acquired")
	}
	ctx := context.Background()

	if err := s.ApplyPortfolioRevalued(ctx, env("e1"), revalued("PORT-X")); !errors.Is(err, state.ErrNotOwned) {
		t.Errorf("ApplyPortfolioRevalued unowned = %v, want ErrNotOwned", err)
	}
	if err := s.ApplyPositionChanged(ctx, env("e2"), &domainpb.PositionState{
		PortfolioId: "PORT-X", InstrumentId: "ACME", AsOf: offset(0),
	}); !errors.Is(err, state.ErrNotOwned) {
		t.Errorf("ApplyPositionChanged unowned = %v, want ErrNotOwned", err)
	}
	if err := s.ApplyPortfolioSnapshot(ctx, env("e3"), &domainpb.PortfolioSnapshot{
		Portfolio: revalued("PORT-X"),
	}); !errors.Is(err, state.ErrNotOwned) {
		t.Errorf("ApplyPortfolioSnapshot unowned = %v, want ErrNotOwned", err)
	}

	// The refusal must reach a reader as a REFUSAL, not as an empty book.
	if _, err := s.SnapshotOwned("PORT-X"); !errors.Is(err, state.ErrNotOwned) {
		t.Errorf("SnapshotOwned unowned = %v, want ErrNotOwned", err)
	}
	if _, ok := s.Lookup("PORT-X"); ok {
		t.Error("a refused apply must leave nothing in the store")
	}
	if ids := s.IDs(); len(ids) != 0 {
		t.Errorf("IDs()=%v, want empty — an unowned portfolio must not be listed", ids)
	}
	// Restore is the boot path and must not be a back door around the gate.
	err := s.Restore(persist.PortfolioRecord{ID: "PORT-X", BaseCurrency: "USD"}.ToPortfolio(), nil)
	if !errors.Is(err, state.ErrNotOwned) {
		t.Errorf("Restore of unowned portfolio = %v, want ErrNotOwned", err)
	}
	if _, ok := s.Lookup("PORT-X"); ok {
		t.Error("a refused Restore must install nothing")
	}
}

// --- acquire loads durable state at runtime -------------------------------

func TestAcquireLoadsDurableStateAndPreSeedsDedup(t *testing.T) {
	loader := newFakeLoader()
	loader.recs["PORT-A"] = durableRecord("PORT-A")
	s := state.NewStore(state.WithRuntimeOwnership(loader))
	ctx := context.Background()

	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p, err := s.SnapshotOwned("PORT-A")
	if err != nil {
		t.Fatalf("SnapshotOwned after Acquire: %v", err)
	}
	if p.DisplayName() != "Durable Alpha" {
		t.Errorf("DisplayName=%q — acquire did not load the durable record", p.DisplayName())
	}
	if len(p.Positions()) != 1 {
		t.Errorf("positions=%d, want 1 — a replica that gains a portfolio must gain its book, not an empty one", len(p.Positions()))
	}

	// The applied-key tail must come with it, or the events replayed at the
	// handover boundary are double-counted.
	if err := s.ApplyPortfolioRevalued(ctx, env("already-folded"), revalued("PORT-A")); err != nil {
		t.Fatalf("apply of an already-folded key: %v", err)
	}
	p2, err := s.SnapshotOwned("PORT-A")
	if err != nil {
		t.Fatalf("SnapshotOwned: %v", err)
	}
	if p2.DisplayName() != "Durable Alpha" {
		t.Errorf("DisplayName=%q — the pre-seeded dedup window did not skip an already-applied key", p2.DisplayName())
	}
}

func TestAcquireIsIdempotentAndDoesNotClobberLiveState(t *testing.T) {
	loader := newFakeLoader()
	loader.recs["PORT-A"] = durableRecord("PORT-A")
	s := state.NewStore(state.WithRuntimeOwnership(loader))
	ctx := context.Background()

	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := s.ApplyPortfolioRevalued(ctx, env("live-1"), revalued("PORT-A")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("second Acquire must be a no-op, got %v", err)
	}
	if got := loader.callCount(); got != 1 {
		t.Errorf("loader calls=%d, want 1 — a repeat Acquire must not re-read the durable store", got)
	}
	p, err := s.SnapshotOwned("PORT-A")
	if err != nil {
		t.Fatalf("SnapshotOwned: %v", err)
	}
	if p.DisplayName() != "Live" {
		t.Errorf("DisplayName=%q, want Live — a repeat Acquire rolled live state back to the durable record", p.DisplayName())
	}
}

func TestAcquireWithNoDurableRecordOwnsButAnswersUnknown(t *testing.T) {
	s := state.NewStore(state.WithRuntimeOwnership(newFakeLoader()))
	ctx := context.Background()
	if err := s.Acquire(ctx, "PORT-NEW"); err != nil {
		t.Fatalf("Acquire of an unsnapshotted portfolio must succeed, got %v", err)
	}
	if !s.Owns("PORT-NEW") {
		t.Fatal("Acquire must take ownership even with no durable record")
	}
	// Owned with no history reads as UNKNOWN, never as a book holding nothing.
	_, err := s.SnapshotOwned("PORT-NEW")
	if !errors.Is(err, v1.ErrPortfolioNotFound) {
		t.Errorf("SnapshotOwned = %v, want ErrPortfolioNotFound — an owned portfolio with no state must not read as an empty book", err)
	}
	if err := s.ApplyPortfolioRevalued(ctx, env("e1"), revalued("PORT-NEW")); err != nil {
		t.Fatalf("apply after acquire: %v", err)
	}
	if _, err := s.SnapshotOwned("PORT-NEW"); err != nil {
		t.Errorf("SnapshotOwned after first apply: %v", err)
	}
}

func TestFailedAcquireLeavesNothingHalfLoadedOrAnswerable(t *testing.T) {
	loader := newFakeLoader()
	loader.recs["PORT-A"] = durableRecord("PORT-A")
	loader.err = errors.New("postgres: connection refused")
	s := state.NewStore(state.WithRuntimeOwnership(loader))
	ctx := context.Background()

	err := s.Acquire(ctx, "PORT-A")
	if err == nil {
		t.Fatal("Acquire must fail when the durable read fails")
	}
	if s.Owns("PORT-A") {
		t.Error("a failed Acquire must not grant ownership — either loaded and owned, or neither")
	}
	if _, ok := s.Lookup("PORT-A"); ok {
		t.Error("a failed Acquire must install no state")
	}
	if _, serr := s.SnapshotOwned("PORT-A"); !errors.Is(serr, state.ErrNotOwned) {
		t.Errorf("SnapshotOwned after failed Acquire = %v, want ErrNotOwned", serr)
	}
	if aerr := s.ApplyPortfolioRevalued(ctx, env("e1"), revalued("PORT-A")); !errors.Is(aerr, state.ErrNotOwned) {
		t.Errorf("apply after failed Acquire = %v, want ErrNotOwned", aerr)
	}
	// Recovery: once the durable read works, the same call succeeds.
	loader.err = nil
	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("retry Acquire: %v", err)
	}
	if !s.Owns("PORT-A") {
		t.Error("a retried Acquire must take ownership")
	}
}

// --- release -------------------------------------------------------------

func TestReleaseDropsMemoryKeepsDurableAndHandsBackTheTail(t *testing.T) {
	loader := newFakeLoader()
	loader.recs["PORT-A"] = durableRecord("PORT-A")
	s := state.NewStore(state.WithRuntimeOwnership(loader))
	ctx := context.Background()

	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := s.ApplyPortfolioRevalued(ctx, env("live-1"), revalued("PORT-A")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rec, released, err := s.Release("PORT-A")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !released {
		t.Fatal("Release must report the portfolio it dropped")
	}
	// The handed-back record is what the mechanism must persist before the
	// next owner acquires — it carries the applies made since the last save.
	if rec.DisplayName != "Live" {
		t.Errorf("released record DisplayName=%q, want Live — the in-memory tail was not handed back", rec.DisplayName)
	}
	if len(rec.AppliedKeys) == 0 {
		t.Error("released record must carry the dedup tail, or the next owner double-counts the boundary")
	}

	if s.Owns("PORT-A") {
		t.Error("Release must drop ownership")
	}
	if _, ok := s.Lookup("PORT-A"); ok {
		t.Error("Release must drop the in-memory copy")
	}
	if _, serr := s.SnapshotOwned("PORT-A"); !errors.Is(serr, state.ErrNotOwned) {
		t.Errorf("SnapshotOwned after Release = %v, want ErrNotOwned", serr)
	}
	if aerr := s.ApplyPortfolioRevalued(ctx, env("live-2"), revalued("PORT-A")); !errors.Is(aerr, state.ErrNotOwned) {
		t.Errorf("apply after Release = %v, want ErrNotOwned", aerr)
	}

	// Durable state survives: re-acquiring reads the record back.
	if _, _, rerr := s.Release("PORT-A"); rerr != nil {
		t.Errorf("releasing an already-released portfolio must not error, got %v", rerr)
	}
	if err := s.Acquire(ctx, "PORT-A"); err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	p, err := s.SnapshotOwned("PORT-A")
	if err != nil {
		t.Fatalf("SnapshotOwned after re-Acquire: %v", err)
	}
	if p.DisplayName() != "Durable Alpha" {
		t.Errorf("DisplayName=%q — Release lost durable state; Postgres must remain the record", p.DisplayName())
	}
}

// --- concurrency: outcome only, not a race proof --------------------------

func TestConcurrentAcquireInstallsOneCopy(t *testing.T) {
	loader := newFakeLoader()
	loader.recs["PORT-A"] = durableRecord("PORT-A")
	s := state.NewStore(state.WithRuntimeOwnership(loader))
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Acquire(ctx, "PORT-A")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Acquire %d: %v", i, err)
		}
	}
	if !s.Owns("PORT-A") {
		t.Fatal("concurrent Acquire left the portfolio unowned")
	}
	if ids := s.IDs(); len(ids) != 1 {
		t.Errorf("IDs()=%v, want exactly one entry", ids)
	}
}
