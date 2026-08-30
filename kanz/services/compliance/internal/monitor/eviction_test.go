package monitor

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
)

// The bookKey every test here folds into. concentrationRegistry files its
// mandate under tenant t1 / portfolio p1, and positionEvent publishes p1.
var evictionKey = bookKey{tenant: "t1", portfolio: "p1"}

// heldCount reports how many instruments the monitor is holding for key, and
// whether it holds a book for it at all.
func heldCount(m *Monitor, key bookKey) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.books[key]
	if !ok {
		return 0, false
	}
	return len(b.positions), true
}

// TestFlatPositionsAreEvictedFromTheBook is #810's own "verified when": a
// portfolio that opens and flattens N instruments must leave the book at 0, not
// at N.
//
// N IS PAST THE BOUND ON PURPOSE. The bound being asserted is "what the
// portfolio holds now", so the test drives the cumulative count to 64 while the
// live count returns to zero; a book that grows with what was EVER held fails
// here at 64 whatever the eviction rule claims.
func TestFlatPositionsAreEvictedFromTheBook(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(&fakeBus{}), nil, nil)
	ctx := testCtx()
	env := &envelopepb.Envelope{TenantId: "t1"}

	const n = 64
	for i := 0; i < n; i++ {
		if err := m.Handle(ctx, env, positionEvent(t, instName(i), 100, 1000, t0.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("open %s: %v", instName(i), err)
		}
	}
	if got, _ := heldCount(m, evictionKey); got != n {
		t.Fatalf("after opening %d instruments the book holds %d", n, got)
	}
	for i := 0; i < n; i++ {
		if err := m.Handle(ctx, env, positionEvent(t, instName(i), 0, 0, t0.Add(time.Duration(n+i)*time.Second))); err != nil {
			t.Fatalf("flatten %s: %v", instName(i), err)
		}
	}
	if got, _ := heldCount(m, evictionKey); got != 0 {
		t.Errorf("a portfolio that opened and flattened %d instruments retains %d of them; "+
			"heldPositions filters a flat position at read time, so every one of these is "+
			"invisible to every rule AND copied into a fresh book on every position FACT", n, got)
	}
}

// TestReopeningAnEvictedInstrumentRestoresIt. Eviction must not be a tombstone:
// the same instrument traded again is a holding, and the book has to see it.
func TestReopeningAnEvictedInstrumentRestoresIt(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(&fakeBus{}), nil, nil)
	ctx := testCtx()
	env := &envelopepb.Envelope{TenantId: "t1"}

	for _, step := range []struct {
		qty, mv int64
		want    int
	}{
		{100, 1000, 1},
		{0, 0, 0},
		{50, 500, 1},
	} {
		if err := m.Handle(ctx, env, positionEvent(t, "AAPL", step.qty, step.mv, t0.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
		if got, _ := heldCount(m, evictionKey); got != step.want {
			t.Fatalf("after qty=%d mv=%d the book holds %d instruments, want %d", step.qty, step.mv, got, step.want)
		}
	}
}

// TestOnlyAStatedZeroQuantityIsEvicted pins each arm of flatPosition. Every case
// that is NOT a stated flat must stay in the book, because comp's rules reason
// about it: an unstated quantity and an unmarked holding are both refusals, and
// a marked-at-zero position with a real quantity is valued at quantity × the
// live mark.
func TestOnlyAStatedZeroQuantityIsEvicted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   *domainpb.PositionState
		evicted bool
	}{
		{
			name:    "stated zero quantity, marked at zero",
			state:   &domainpb.PositionState{Quantity: decv(0, 0), MarketValue: &commonpb.Money{Amount: decv(0, 0), CurrencyCode: "USD"}},
			evicted: true,
		},
		{
			name:    "stated zero quantity, no market value at all",
			state:   &domainpb.PositionState{Quantity: decv(0, 0)},
			evicted: true,
		},
		{
			name:    "no quantity stated: a producer that did not say, not a flat position",
			state:   &domainpb.PositionState{MarketValue: &commonpb.Money{Amount: decv(0, 0), CurrencyCode: "USD"}},
			evicted: false,
		},
		{
			name:    "real quantity marked at zero: equity values it at quantity x the live mark",
			state:   &domainpb.PositionState{Quantity: decv(100, 0), MarketValue: &commonpb.Money{Amount: decv(0, 0), CurrencyCode: "USD"}},
			evicted: false,
		},
		{
			name:    "zero quantity contradicted by a non-zero market value",
			state:   &domainpb.PositionState{Quantity: decv(0, 0), MarketValue: &commonpb.Money{Amount: decv(5, 0), CurrencyCode: "USD"}},
			evicted: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(&fakeBus{}), nil, nil)
			ps := proto.Clone(tc.state).(*domainpb.PositionState)
			ps.PortfolioId = "p1"
			ps.InstrumentId = "AAPL"
			ps.AsOf = timestamppb.New(t0.Add(time.Minute))
			payload, err := proto.Marshal(ps)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, payload); err != nil {
				t.Fatal(err)
			}
			got, _ := heldCount(m, evictionKey)
			want := 1
			if tc.evicted {
				want = 0
			}
			if got != want {
				t.Fatalf("book holds %d instruments, want %d", got, want)
			}
		})
	}
}

// TestAnIdleEmptyBookIsEvictedWholeAndALiveOneNeverIs. The (tenant, portfolio)
// entry is the second half of #810: evicting the positions inside it leaves the
// key, its last status and its ungoverned flag behind forever.
//
// The clock is driven rather than waited on, and the sweep is reached through
// Handle — the production path — rather than by calling it directly.
func TestAnIdleEmptyBookIsEvictedWholeAndALiveOneNeverIs(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(&fakeBus{}), nil, nil)
	now := t0
	m.now = func() time.Time { return now }
	ctx := testCtx()
	env := &envelopepb.Envelope{TenantId: "t1"}

	// Three books, because the sweep runs at the TOP of the fold and would
	// otherwise be invisible on the one book the triggering FACT refills:
	//   dead      — opens and flattens, so it is the eviction candidate.
	//   live      — receives the FACT that drives the sweep.
	//   bystander — holds a position and is never touched again, which is the
	//               only book that can show a sweep taking a live one.
	// Separate tenants because positionEvent always names portfolio p1.
	dead := bookKey{tenant: "dead", portfolio: "p1"}
	bystander := bookKey{tenant: "bystander", portfolio: "p1"}
	live := evictionKey
	for _, seed := range []struct {
		tenant  string
		qty, mv int64
	}{{"dead", 100, 1000}, {"dead", 0, 0}, {"bystander", 100, 1000}, {"t1", 100, 1000}} {
		if err := m.Handle(ctx, &envelopepb.Envelope{TenantId: seed.tenant},
			positionEvent(t, "MSFT", seed.qty, seed.mv, t0)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := heldCount(m, dead); !ok {
		t.Fatal("a book that emptied moments ago was dropped immediately — bookIdleRetention is not being honoured")
	}

	// Past the retention, and a FACT arrives so the lazy sweep runs.
	now = now.Add(bookIdleRetention + time.Minute)
	if err := m.Handle(ctx, env, positionEvent(t, "MSFT", 100, 1000, t0)); err != nil {
		t.Fatal(err)
	}
	if _, ok := heldCount(m, dead); ok {
		t.Errorf("a book that has held nothing for %v is still keyed in m.books, "+
			"along with its last status and its ungoverned flag", bookIdleRetention)
	}
	for _, k := range []bookKey{live, bystander} {
		if got, ok := heldCount(m, k); !ok || got != 1 {
			t.Errorf("the sweep took %s's book, which still holds a position (held=%d, present=%v); "+
				"this map is the monitor's only copy of the holdings, so that is a silently "+
				"under-reported book, not reclaimed memory", k.tenant, got, ok)
		}
	}
}

// TestAFlattenedPortfolioIsNotReportedAsUnverifiableLeverage is the trap #810's
// eviction opens and the reason Handle refuses to evaluate an empty book.
//
// Retained flats kept the book non-empty, so it always had a base currency and
// always valued. A genuinely empty book has neither: comp.equityFromMarks
// refuses ("no base currency, so nothing can be summed into equity") and
// LeverageRule fails closed. Evaluating it would emit a ComplianceBreach — which
// AUTO-01 halts and escalates on — because the fund stopped holding anything.
//
// Both paths into evaluate are driven: the FACT that empties the book, and the
// sweep that walks every book afterwards.
func TestAFlattenedPortfolioIsNotReportedAsUnverifiableLeverage(t *testing.T) {
	l := leveredBook(t, 3, 0, 100, -50_000) // gross 100k / equity 50k = 2.0, under a cap of 3
	l.hold(t, 90_000)
	if len(l.bus.events) != 0 {
		t.Fatalf("the held book was expected to pass its leverage cap, got %d breach FACTs", len(l.bus.events))
	}

	if err := l.mon.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 0, 0, t0)); err != nil {
		t.Fatal(err)
	}
	if n := len(l.bus.events); n != 0 {
		t.Fatalf("closing the last position emitted %d breach FACT(s): a portfolio that holds "+
			"nothing was reported as one whose leverage cannot be verified", n)
	}
	if err := l.mon.ReevaluateAll(context.Background()); err != nil {
		t.Fatalf("ReevaluateAll: %v", err)
	}
	if n := len(l.bus.events); n != 0 {
		t.Fatalf("the sweep emitted %d breach FACT(s) for a portfolio that holds nothing — "+
			"Reevaluate is reading the book's KEY rather than its holdings", n)
	}
}

// TestRecordStatusTreatsAForgottenBookAsANewBreach pins the direction
// recordStatus takes when the sweep has already dropped the book: this IS a
// transition, so the breach is emitted. The branch is not reachable through
// Handle — applyLocked re-creates the book before evaluate ever runs — so it is
// asserted directly rather than left as an unexercised defensive arm.
func TestRecordStatusTreatsAForgottenBookAsANewBreach(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), comp.NewMandateRegistry(), nil, nil, nil, nil)
	forgotten := bookKey{tenant: "t1", portfolio: "gone"}
	if !m.recordStatus(forgotten, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH) {
		t.Error("a breach on a book the sweep has dropped was reported as no transition, so it " +
			"would be swallowed; the safe direction is a duplicate FACT, never a lost one")
	}
	if m.recordStatus(forgotten, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS) {
		t.Error("a PASS was reported as an entry into breach")
	}
}

// TestBreachIsReEmittedWhenAFlattenedPortfolioReopens is the one property that
// makes "do not evaluate an empty book" safe. A portfolio that was BREACHING
// when its last position closed is not evaluated again, so unless the verdict is
// forgotten with the holding, recordStatus sees BREACH → BREACH on the re-open
// and emits nothing. The clock does not move here: this is the in-window case,
// with the book still keyed and only its status cleared.
//
// TestBreachIsReEmittedAfterTheBookIsSwept is the same property once the sweep
// has taken the whole entry.
func TestBreachIsReEmittedWhenAFlattenedPortfolioReopens(t *testing.T) {
	assertBreachSurvivesAFlatten(t, 0)
}

func TestBreachIsReEmittedAfterTheBookIsSwept(t *testing.T) {
	assertBreachSurvivesAFlatten(t, bookIdleRetention+time.Minute)
}

func assertBreachSurvivesAFlatten(t *testing.T, idle time.Duration) {
	t.Helper()
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(fb), nil, nil)
	now := t0
	m.now = func() time.Time { return now }
	ctx := testCtx()
	env := &envelopepb.Envelope{TenantId: "t1"}

	// One instrument at 100% of the book breaches the 60% concentration cap.
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 1 {
		t.Fatalf("expected the seed breach, got %d events", len(fb.events))
	}
	// Same book again: still breaching, no transition, no second emit.
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 100000, t0.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 1 {
		t.Fatalf("a book that was already breaching re-emitted: %d events", len(fb.events))
	}
	// Flatten, wait out the given idle time, then re-open into the same breach.
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 0, 0, t0.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	now = now.Add(idle)
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 100000, t0.Add(4*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 2 {
		t.Errorf("a portfolio that breached, flattened and breached again emitted %d FACTs, want 2 — "+
			"a re-entry into breach after the book went flat must not be swallowed", len(fb.events))
	}
	if got := fb.events[len(fb.events)-1].Payload.(*compliancepb.ComplianceBreach).GetResult().GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("re-emitted FACT does not carry BREACH: %v", got)
	}
}

// TestASweptEmptyBookIsNotReEvaluated. Reevaluate must read the HOLDINGS, not
// the key: an empty book passes every concentration limit there is (EXEC-M18),
// so evaluating one would report a portfolio holding nothing as clean.
func TestAnEmptyBookIsNotReEvaluated(t *testing.T) {
	fb := &fakeBus{}
	rec := &recordingRecorder{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(fb), rec, nil)
	ctx := testCtx()
	env := &envelopepb.Envelope{TenantId: "t1"}

	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 0, 0, t0.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	before := len(rec.records)
	if err := m.ReevaluateAll(ctx); err != nil {
		t.Fatalf("ReevaluateAll: %v", err)
	}
	if got := len(rec.records); got != before {
		t.Errorf("the sweep evaluated a portfolio that holds nothing (%d new decision records)", got-before)
	}
}

// TestSnapshotAllocatesOncePerHolding is the named assertion behind the
// snapshot half of #810. The copy runs on EVERY position FACT, so an allocation
// per holding is the floor and anything above it is per-event garbage: the
// version this replaced grew book.Positions from nil (reallocating and
// re-copying as it doubled) and scanned the map twice more for a currency it
// computed and threw away.
func TestSnapshotAllocatesOncePerHolding(t *testing.T) {
	const n = 50
	m := NewMonitor(comp.NewEngine(nil), comp.NewMandateRegistry(), nil, nil, nil, nil)
	seedBook(m, evictionKey, n)
	got := testing.AllocsPerRun(100, func() { _ = m.snapshot(evictionKey) })
	// One per holding for the running NAV sum, plus a small constant for the
	// Book, the positions slice and the NAV Money.
	if max := float64(n + 5); got > max {
		t.Errorf("snapshot of a %d-holding book allocates %.0f times, want at most %.0f — "+
			"the per-FACT copy is growing its slice from nil or re-scanning the map", n, got, max)
	}
}

// TestSnapshotNamesOneCurrency. Go randomizes map iteration per range, so
// computing "the first currency seen" twice could name two different currencies
// on one snapshot — a book whose BaseCurrency and NAV disagree is one
// comp.JoinEquity would refuse to value for a reason nobody could reproduce.
func TestSnapshotNamesOneCurrency(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), comp.NewMandateRegistry(), nil, nil, nil, nil)
	m.mu.Lock()
	m.books[evictionKey] = &book{positions: map[string]comp.Position{
		"AAPL": {InstrumentID: "AAPL", Quantity: decv(1, 0), MarketValue: &commonpb.Money{Amount: decv(10, 0), CurrencyCode: "USD"}},
		"SAP":  {InstrumentID: "SAP", Quantity: decv(1, 0), MarketValue: &commonpb.Money{Amount: decv(10, 0), CurrencyCode: "EUR"}},
	}}
	m.mu.Unlock()
	for i := 0; i < 200; i++ {
		b := m.snapshot(evictionKey)
		if b.BaseCurrency != b.NAV.GetCurrencyCode() {
			t.Fatalf("one snapshot named two currencies: BaseCurrency=%q NAV=%q", b.BaseCurrency, b.NAV.GetCurrencyCode())
		}
	}
}

func instName(i int) string {
	return string(rune('A'+i/26)) + string(rune('A'+i%26)) + "X"
}
