package monitor

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/treasury"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// The old Reevaluate checked held, unlocked, then captured a different book.
// Drive that exact state sequence at the capture boundary without a permanent
// timing hook: the formerly true eligibility cannot authorize an empty snapshot.
func TestReevaluationSnapshotEligibilityIsCaptured(t *testing.T) {
	for _, state := range []string{"absent", "last-flat", "evicted", "held", "canonical-unknown", "legacy-unknown"} {
		t.Run(state, func(t *testing.T) {
			l := leveredBook(t, 3, 0, 100, -50_000)
			if state != "absent" && state != "canonical-unknown" && state != "legacy-unknown" {
				l.hold(t, 90_000)
				l.mon.mu.Lock()
				wasHeld := len(l.mon.books[evictionKey].positions) > 0
				l.mon.mu.Unlock()
				if !wasHeld {
					t.Fatal("fixture did not reach the former held preflight")
				}
			}
			switch state {
			case "last-flat", "evicted":
				if err := l.mon.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", 0, 0, t0)); err != nil {
					t.Fatal(err)
				}
				if state == "evicted" {
					l.mon.now = clockAt(t0.Add(bookIdleRetention + 2*time.Minute))
					l.mon.mu.Lock()
					l.mon.gcBooksLocked()
					l.mon.mu.Unlock()
					if _, ok := heldCount(l.mon, evictionKey); ok {
						t.Fatal("fixture did not evict the idle flat book")
					}
				}
			case "canonical-unknown", "legacy-unknown":
				key := "p1"
				if state == "canonical-unknown" {
					key = subject.PositionFor("t1", "p1", "AAPL")
				}
				if err := positionIdentityFeed(t, l.mon, "t1", "p1", "", 1, 100, key); !errors.Is(err, errPositionIdentity) {
					t.Fatalf("fixture did not establish UNKNOWN: %v", err)
				}
			}
			captured := l.mon.snapshot(evictionKey)
			want := state == "held" || state == "canonical-unknown" || state == "legacy-unknown"
			if (captured != nil) != want {
				t.Fatalf("captured eligibility=%v, want %v; a prior held check must not authorize an empty book", captured != nil, want)
			}
			if !want {
				return
			}
			if len(captured.Positions) != 1 {
				t.Fatalf("capture lost the holding or UNKNOWN marker: %+v", captured.Positions)
			}
			if state != "held" {
				if err := l.mon.Reevaluate(context.Background(), "t1", "p1"); !errors.Is(err, errPositionIdentity) {
					t.Fatalf("UNKNOWN became an ordinary flat no-op: %v", err)
				}
			}
		})
	}
}

func TestReevaluationFlatAndAbsentBooksHaveNoEffects(t *testing.T) {
	for _, path := range []string{"one", "all"} {
		t.Run(path, func(t *testing.T) {
			s := &positionTimeSources{registry: leverageRegistry(t, 3, 0)}
			fb, rec := &fakeBus{}, &recordingRecorder{}
			m := NewMonitor(nil, s, s, NewEmitter(fb), rec, nil, WithCashSource(s), WithMarkSource(s),
				WithCashDragObserver(func(string, string, treasury.Drag) { s.drag++ }))
			m.now = clockAt(t0)
			for _, qty := range []int64{1_000, 0} {
				if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", qty, qty*100, t0)); err != nil {
					t.Fatal(err)
				}
			}
			before, sources := positionTimeBooks(m), *s
			for _, key := range []bookKey{evictionKey, {tenant: "other", portfolio: "p1"}, {tenant: "t1", portfolio: "absent"}} {
				var err error
				if path == "all" {
					err = m.ReevaluateAll(context.Background())
				} else {
					err = m.Reevaluate(context.Background(), key.tenant, key.portfolio)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(before, positionTimeBooks(m)) || !reflect.DeepEqual(sources, *s) || len(fb.events)+len(rec.records) != 0 {
				t.Fatal("ineligible reevaluation read valuation/policy, measured cash drag, changed state, or emitted a decision/breach")
			}
		})
	}
}

func TestReevaluationHeldSnapshotRemainsEligibleAfterLaterFlat(t *testing.T) {
	l := leveredBook(t, 3, 0, 100, -50_000)
	rec := &recordingRecorder{}
	l.mon.recorder = rec
	l.hold(t, 90_000)
	foldCash(t, l.cash, "p1", -70_000, t0) // gross 100k / equity 30k > 3
	captured := l.mon.snapshot(evictionKey)
	if captured == nil || captured.NAVBasis != comp.NAVBasisEquity {
		t.Fatal("fixture did not capture the genuinely held marked book")
	}
	if err := l.mon.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", 0, 0, t0)); err != nil {
		t.Fatal(err)
	}
	observed := t0.Add(123456789)
	if err := l.mon.evaluate(bus.WithTenantID(context.Background(), "foreign-context"), evictionKey, captured,
		compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE, observed); err != nil {
		t.Fatal(err)
	}
	if len(l.bus.events) != 1 || len(rec.records) != 1 {
		t.Fatal("a later flat suppressed a genuine breach of the captured held book")
	}
	br := l.bus.events[0].Payload.(*compliancepb.ComplianceBreach)
	if br.GetTrigger() != compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE || !br.GetDetectedAt().AsTime().Equal(observed) ||
		!br.GetResult().GetEvaluatedAt().AsTime().Equal(observed) || rec.records[0].TenantID != "t1" ||
		rec.records[0].Phase != comp.PhasePostTrade || rec.records[0].Result.GetPortfolioId() != "p1" {
		t.Fatal("held-snapshot breach lost its observation time, trigger, portfolio, tenant, or decision attribution")
	}
	if got, _ := heldCount(l.mon, evictionKey); got != 0 {
		t.Fatal("evaluating a captured book resurrected the flat holding")
	}
}

// With real synchronized valuation folds, race CI also exercises independent
// live position and reevaluation goroutines. The deterministic test above is the
// regression oracle; this loop does not claim to force a particular scheduling.
type reevaluationConcurrentRecorder struct {
	mu sync.Mutex
	n  int
}

func (r *reevaluationConcurrentRecorder) Record(context.Context, comp.DecisionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil
}

func TestReevaluationConcurrentHealthyHoldingsAndFlats(t *testing.T) {
	l := leveredBook(t, 3, 0, 100, -50_000)
	rec := &reevaluationConcurrentRecorder{}
	l.mon.emitter, l.mon.recorder = nil, rec
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() {
		for range 100 {
			for _, qty := range []int64{1_000, 0} {
				if err := l.mon.Handle(ctx, &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", qty, qty*100, t0)); err != nil {
					errs <- err
					return
				}
			}
		}
	})
	wg.Go(func() {
		for range 200 {
			if err := l.mon.ReevaluateAll(ctx); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got, _ := heldCount(l.mon, evictionKey); got != 0 {
		t.Fatalf("last explicit flat left %d holdings", got)
	}
	if rec.n != 0 {
		t.Fatal("healthy captured holdings or genuine flats produced a phantom breach")
	}
}
