package orderview

// THE STORE'S CONDITIONAL WRITE, HELD TO THE SAME CONTRACT ON BOTH BACKENDS (#934).
//
// Memory establishes "nothing changed underneath" with a counter under a mutex;
// Postgres establishes it with an equality on the stored bytes inside an UPDATE.
// Those are not the same mechanism and neither one is evidence for the other, so
// every property below runs against BOTH — the Postgres half against a real
// engine, because a double reports success for a predicate the database would
// have refused. #905's own history is the argument: a green in-memory suite hid a
// duplicate venue cancel that the first Postgres-gated run caught.
//
// THE DECISIVE CASE IS RefusesWhenOnlyTheQuantityMoved, and it is here because
// the shape that suggests itself first — a compare-and-set on the order's STATUS
// — passes every other case in this file. Both writers that need a conditional
// write write a status CHANGE, so a status predicate does catch a racing writer
// that also moved the status. It does not catch the ordinary one: a partially
// filling order takes another fill and the venue's own report writes
// PARTIALLY_FILLED over PARTIALLY_FILLED with a larger filled_quantity. A status
// CAS applies, the merge is still built on the pre-fill read, and the quantity
// the exchange reported is gone — permanently, in venue_orders, which is never
// pruned.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// eachBackend runs fn against every Store this package ships. The Postgres leg
// SKIPS without TEST_POSTGRES_URL and FATALS on an RLS-exempt role — newPGFixture
// owns both rules and this file does not restate them.
func eachBackend(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("Memory", func(t *testing.T) { fn(t, NewMemory()) })
	t.Run("Postgres", func(t *testing.T) {
		f := newPGFixture(t)
		s, _ := f.storeAs(t, "acme")
		fn(t, s)
	})
}

// partiallyFilled is an order the venue has reported a partial fill on: the state
// both racing writers below are working from.
func partiallyFilled(id string, filled, leaves int64) *orderpb.OrderState {
	st := routed(id)
	st.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	st.OrderedQuantity = dec(150_000_000, -8) // 1.5
	st.FilledQuantity = dec(filled, -8)
	st.LeavesQuantity = dec(leaves, -8)
	return st
}

func TestAConditionalWriteAppliesOnlyToTheValueItRead(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()

		t.Run("AppliesWhileNothingChanged", func(t *testing.T) {
			id := "o-quiet"
			if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			cur, rev, ok, err := s.Get(ctx, id)
			if err != nil || !ok {
				t.Fatalf("Get: ok=%v err=%v", ok, err)
			}
			next := proto.Clone(cur).(*orderpb.OrderState)
			next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			applied, err := s.RecordIf(ctx, next, rev)
			if err != nil {
				t.Fatalf("RecordIf: %v", err)
			}
			if !applied {
				t.Fatal("a conditional write refused against a value NOTHING had changed — the " +
					"writer would retry forever and the cancel would never land")
			}
			got, _, _, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}
			if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
				t.Fatalf("status after an applied conditional write = %v, want CANCELLED", got.GetStatus())
			}
		})

		t.Run("RefusesWhenTheStatusMoved", func(t *testing.T) {
			id := "o-status-moved"
			if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			stale, rev, _, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// The venue's own execution report lands in the gap.
			filled := partiallyFilled(id, 150_000_000, 0)
			filled.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
			if err := s.Record(ctx, filled); err != nil {
				t.Fatalf("racing write: %v", err)
			}

			next := proto.Clone(stale).(*orderpb.OrderState)
			next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			applied, err := s.RecordIf(ctx, next, rev)
			if err != nil {
				t.Fatalf("RecordIf: %v", err)
			}
			if applied {
				t.Fatal("a conditional write applied over a value that had moved — the venue's " +
					"FILLED verdict was replaced by a merge built on the pre-fill read")
			}
		})

		// THE ONE A STATUS COMPARE-AND-SET PASSES. Nothing about the status
		// changed; the exchange reported another fill on the same working order.
		t.Run("RefusesWhenOnlyTheQuantityMoved", func(t *testing.T) {
			id := "o-quantity-moved"
			if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			stale, rev, _, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := s.Record(ctx, partiallyFilled(id, 70_000_000, 80_000_000)); err != nil {
				t.Fatalf("racing write: %v", err)
			}

			next := proto.Clone(stale).(*orderpb.OrderState)
			next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			applied, err := s.RecordIf(ctx, next, rev)
			if err != nil {
				t.Fatalf("RecordIf: %v", err)
			}
			if applied {
				t.Fatal("a conditional write applied over an order whose STATUS was unchanged and " +
					"whose FILLED QUANTITY had moved from 0.4 to 0.7. That is the ordinary case — a " +
					"partially filling order taking another fill — and it is exactly the one a " +
					"compare-and-set on the status cannot see. The write that follows carries the " +
					"stale 0.4, and this adapter can no longer say what the venue filled")
			}
			// And the value the venue reported is still the one in the store.
			got, _, _, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}
			if !proto.Equal(got.GetFilledQuantity(), dec(70_000_000, -8)) {
				t.Fatalf("filled_quantity = %v, want 0.7 (coefficient 70000000, exponent -8)",
					got.GetFilledQuantity())
			}
		})

		t.Run("SeedsOnlyWhileTheOrderIsStillAbsent", func(t *testing.T) {
			id := "o-absent"
			cur, rev, ok, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if ok || cur != nil {
				t.Fatalf("Get for an order nothing recorded returned ok=%v", ok)
			}
			// Another writer seeds it first.
			if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
				t.Fatalf("racing seed: %v", err)
			}
			applied, err := s.RecordIf(ctx, routed(id), rev)
			if err != nil {
				t.Fatalf("RecordIf: %v", err)
			}
			if applied {
				t.Fatal("a conditional SEED applied over a row another writer had already " +
					"inserted — absent is a value like any other, and two writers racing to seed " +
					"one order must not both win")
			}
			// The same token DOES apply while the order really is absent.
			fresh := "o-absent-2"
			_, freshRev, _, err := s.Get(ctx, fresh)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			applied, err = s.RecordIf(ctx, routed(fresh), freshRev)
			if err != nil {
				t.Fatalf("RecordIf: %v", err)
			}
			if !applied {
				t.Fatal("a conditional seed refused for an order that was, and stayed, absent — a " +
					"cancel arriving after a restart would then record nothing at all")
			}
		})

		// AN UNMINTED TOKEN IS REFUSED, NEVER TREATED AS A WILDCARD. A caller
		// holding the zero Revision never read anything, so the store cannot
		// establish what the write would be overwriting — and an unknown on the
		// capital path's own record of what the venue did fails closed.
		t.Run("RefusesARevisionItNeverMinted", func(t *testing.T) {
			id := "o-unminted"
			if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			applied, err := s.RecordIf(ctx, routed(id), Revision{})
			if !errors.Is(err, ErrUnmintedRevision) {
				t.Fatalf("RecordIf with the zero Revision = (%v, %v), want ErrUnmintedRevision — a "+
					"token nobody read is not permission to overwrite anything", applied, err)
			}
			if applied {
				t.Fatal("RecordIf reported it applied a write it also reported an error for")
			}
			got, _, _, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}
			if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
				t.Fatalf("status = %v after a refused conditional write, want PARTIALLY_FILLED — "+
					"the refusal wrote anyway", got.GetStatus())
			}
		})
	})
}

// A REVISION OUTLIVES ITS ENTRY, AND THE WRITE MUST NOT. Memory drops a terminal
// order after DefaultTerminalRetention, so a writer holding a revision read
// before the sweep is holding a token for a value that no longer exists — and the
// entry the write would create is one nothing decided to create. Presence is half
// the condition for exactly this: without it the seq comparison is simply skipped
// when the entry is gone, and the stale write lands as a fresh seed.
//
// Memory only, deliberately. venue_orders is never pruned, so Postgres has no
// path that removes a row — and the conditional UPDATE matches nothing anyway
// when there is none.
func TestAConditionalWriteRefusesAPresentRevisionAfterEvictionDroppedTheEntry(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)

	const id = "o-evicted"
	if err := m.Record(ctx, cancelled(id)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale, rev, ok, err := m.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}

	// Past the retention, and a write elsewhere runs the amortized sweep.
	clk.add(2 * DefaultTerminalRetention)
	if err := m.Record(ctx, working("o-other")); err != nil {
		t.Fatalf("unrelated write: %v", err)
	}
	if _, _, stillThere, gerr := m.Get(ctx, id); gerr != nil || stillThere {
		t.Fatalf("the terminal order was not evicted (found=%v err=%v) — this test is asserting "+
			"nothing", stillThere, gerr)
	}

	applied, err := m.RecordIf(ctx, stale, rev)
	if err != nil {
		t.Fatalf("RecordIf: %v", err)
	}
	if applied {
		t.Fatal("a conditional write applied against a revision whose entry had been evicted — it " +
			"re-created an order the store had deliberately forgotten, and reset the retention " +
			"clock on it")
	}
}

// A TOKEN FROM THE OTHER BACKEND IS NOT A TOKEN. Memory's counter and Postgres's
// state bytes are unrelated quantities, so a revision crossing between them would
// be compared against a field the minting store never set — and would apply. Both
// stores refuse instead, which is the same fail-closed rule as the zero value.
func TestAConditionalWriteRefusesARevisionTheOtherBackendMinted(t *testing.T) {
	ctx := context.Background()
	f := newPGFixture(t)
	pg, _ := f.storeAs(t, "acme")
	mem := NewMemory()

	const id = "o-crossed"
	if err := pg.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
		t.Fatalf("seed postgres: %v", err)
	}
	if err := mem.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	_, pgRev, _, err := pg.Get(ctx, id)
	if err != nil {
		t.Fatalf("postgres Get: %v", err)
	}
	_, memRev, _, err := mem.Get(ctx, id)
	if err != nil {
		t.Fatalf("memory Get: %v", err)
	}

	if applied, err := pg.RecordIf(ctx, routed(id), memRev); !errors.Is(err, ErrUnmintedRevision) || applied {
		t.Fatalf("Postgres.RecordIf against a Memory revision = (%v, %v), want ErrUnmintedRevision", applied, err)
	}
	if applied, err := mem.RecordIf(ctx, routed(id), pgRev); !errors.Is(err, ErrUnmintedRevision) || applied {
		t.Fatalf("Memory.RecordIf against a Postgres revision = (%v, %v), want ErrUnmintedRevision", applied, err)
	}
}

// interleaved is a Store that lets a test commit a write in the gap between a
// read-modify-write's read and its write, deterministically and in one goroutine.
// Racing two goroutines and hoping would prove nothing repeatable, and -race
// would not see it either: a lost update is a correct-looking interleaving, not a
// data race.
type interleaved struct {
	Store
	gets int
	// during runs immediately after the Nth Get has read the store and before
	// its value reaches the caller, so the caller decides against a value the
	// store no longer holds.
	during func()
	on     int
}

func (i *interleaved) Get(ctx context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error) {
	st, rev, ok, err := i.Store.Get(ctx, orderID)
	i.gets++
	if i.gets == i.on && i.during != nil {
		i.during()
	}
	return st, rev, ok, err
}

// UPDATE RE-READS AND RE-DECIDES, IT DOES NOT RE-APPLY. The decision the caller
// made against the value that lost is not the decision it would have made against
// the value that won, and a retry loop that re-sent the first one would write the
// stale merge on the second attempt — the same lost update, one round later.
func TestUpdateReDecidesAgainstTheValueThatWon(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		const id = "o-redecide"
		if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed: %v", err)
		}
		view := &interleaved{Store: s, on: 1, during: func() {
			if err := s.Record(ctx, partiallyFilled(id, 70_000_000, 80_000_000)); err != nil {
				t.Errorf("racing write: %v", err)
			}
		}}

		var seen []int64
		err := Update(ctx, view, id, func(cur *orderpb.OrderState, found bool) (*orderpb.OrderState, error) {
			if !found {
				return nil, fmt.Errorf("order %s vanished", id)
			}
			seen = append(seen, cur.GetFilledQuantity().GetCoefficient())
			next := proto.Clone(cur).(*orderpb.OrderState)
			next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			return next, nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if len(seen) != 2 || seen[0] != 40_000_000 || seen[1] != 70_000_000 {
			t.Fatalf("the decision saw filled quantities %v, want [40000000 70000000] — it must be "+
				"asked again, against the value that actually won, not have its first answer "+
				"re-sent", seen)
		}
		got, _, ok, err := s.Get(ctx, id)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if !proto.Equal(got.GetFilledQuantity(), dec(70_000_000, -8)) {
			t.Fatalf("filled_quantity = %v after the retry, want 0.7 — the write that landed was "+
				"built on the stale read", got.GetFilledQuantity())
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
			t.Fatalf("status = %v, want CANCELLED — the write never landed at all", got.GetStatus())
		}
	})
}

// A DECISION TO WRITE NOTHING IS A DECISION. Update must not turn it into a write,
// and must not treat it as a failure to retry.
func TestUpdateWritesNothingWhenTheDecisionIsNil(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		const id = "o-noop"
		if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed: %v", err)
		}
		calls := 0
		err := Update(ctx, s, id, func(*orderpb.OrderState, bool) (*orderpb.OrderState, error) {
			calls++
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if calls != 1 {
			t.Fatalf("the decision was asked %d times for a no-op, want 1", calls)
		}
		got, _, _, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
			t.Fatalf("status = %v after a no-op decision, want PARTIALLY_FILLED", got.GetStatus())
		}
	})
}

// alwaysLoses refuses every conditional write without failing, which is what a
// store under permanent contention looks like from the caller's side.
type alwaysLoses struct{ Store }

func (alwaysLoses) RecordIf(context.Context, *orderpb.OrderState, Revision) (bool, error) {
	return false, nil
}

// THE BOUND IS A REFUSAL, NOT A SILENT SUCCESS. Update gives up after
// UpdateAttempts rounds and says so. The alternative shapes are both worse: an
// unbounded retry stalls a websocket read loop or a gRPC handler the OMS is
// waiting on, and returning nil would report a write that did not happen.
func TestUpdateRefusesRatherThanRetryingForever(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		const id = "o-contended"
		if err := s.Record(ctx, partiallyFilled(id, 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed: %v", err)
		}
		calls := 0
		err := Update(ctx, alwaysLoses{Store: s}, id, func(cur *orderpb.OrderState, _ bool) (*orderpb.OrderState, error) {
			calls++
			next := proto.Clone(cur).(*orderpb.OrderState)
			next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			return next, nil
		})
		if !errors.Is(err, ErrContended) {
			t.Fatalf("Update under permanent contention = %v, want ErrContended — a conditional "+
				"write that gives up quietly reports a write that did not happen", err)
		}
		if calls != UpdateAttempts {
			t.Fatalf("the decision was asked %d times, want UpdateAttempts (%d)", calls, UpdateAttempts)
		}
		got, _, _, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
			t.Fatalf("status = %v, want PARTIALLY_FILLED — the refusal wrote anyway", got.GetStatus())
		}
	})
}

// THE MEASUREMENT, NOT AN ASSERTION OF INTENT. Two writers advance the same order
// concurrently; every write must be visible in the total. Against Postgres this
// runs over real pooled connections, so it is a statement about the SQL predicate
// under the engine's own row locking — not about one process's mutex.
//
// Before the conditional write this counted short, by however many writes landed
// inside another writer's read-to-write window.
func TestConcurrentUpdatesLoseNoWrites(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		const (
			id      = "o-concurrent"
			writers = 2
			each    = 20
		)
		if err := s.Record(ctx, partiallyFilled(id, 0, 150_000_000)); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, writers*each)
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					err := Update(ctx, s, id, func(cur *orderpb.OrderState, _ bool) (*orderpb.OrderState, error) {
						next := proto.Clone(cur).(*orderpb.OrderState)
						next.FilledQuantity = dec(cur.GetFilledQuantity().GetCoefficient()+1_000_000, -8)
						return next, nil
					})
					if err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("Update: %v", err)
		}

		got, _, _, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		want := int64(writers * each * 1_000_000)
		if got.GetFilledQuantity().GetCoefficient() != want {
			t.Fatalf("filled_quantity coefficient = %d after %d concurrent writes, want %d — %d "+
				"writes landed inside another writer's read-to-write window and were overwritten",
				got.GetFilledQuantity().GetCoefficient(), writers*each, want,
				(want-got.GetFilledQuantity().GetCoefficient())/1_000_000)
		}
	})
}
