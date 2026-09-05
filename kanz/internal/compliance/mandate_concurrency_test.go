package compliance

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// The registry is WRITTEN from the mandate subscription's goroutine and READ
// from the order-admission path, and in every service that hosts both they are
// different goroutines by construction — the OMS main's
// consumer.SubscribeBroadcastReady goroutine, reaching Put through
// MandateLoader.Apply, against the same mandateReg handed to buildPreTradeGate
// as preTradeDeps.Mandates and evaluated on the order-COMMAND consumer.
// services/compliance and services/optimization wire the identical pair.
//
// Nothing in this package exercised that. The only test that called both,
// TestARestartedGateArmsItselfWithEveryMandateInForce, calls registry.Mandate
// INSIDE the handler closure that has just run mandateConsumer.Handle, so the
// two are serialised on one goroutine and the interleaving is unreachable by
// construction — which is why a green
// -race CI never reported #1048. A race detector reports races that HAPPEN; it
// cannot report one no test provokes. These three tests provoke it.
//
// They assert the CORRECTNESS half rather than leaning on -race, because -race
// needs cgo and does not run on the Windows box: a reader must never observe a
// version array mid-mutation, and the version it selects must be the one the
// registry's own resolution rule names. That half fails on the pre-fix code with
// the detector switched off, which is the point — the defect is not merely
// undefined behaviour, it is the gate evaluating an order against a superseded
// or not-yet-in-force mandate and then stamping that version into
// ComplianceResult.mandate_version as the authority that permitted it.

// hinge is the version the concurrency tests republish. Its effective_at
// ALTERNATES between a date after every other version and a date before every
// other version, which is what makes Put's sort.Slice genuinely permute the
// array: republishing a version whose position does not move leaves an already
// sorted slice, and Go's insertion sort then performs no writes at all, so the
// window this issue is about never opens.
//
// An operator moving a mandate's effective_at on republish is not contrived —
// the digest the two-signature flow covers includes effective_at precisely
// because it is allowed to change, which is the reason Put re-sorts on its
// idempotent-replace path rather than only on append.
const hingeVersion = uint64(4)

// concurrencyFixture builds the registry the three tests below share.
//
// The dates are chosen so the SELECTED VERSION IS INVARIANT across both states
// of the hinge, which is what turns a race into an assertable wrong answer
// rather than a benign choice between two legal serialisations:
//
//	state A (hinge at +9h):  sorted [v1 +1h, v2 +2h, v3 +3h, v4 +9h]
//	                         asOf +2h30m matches {v1, v2} -> selects v2
//	state B (hinge at +30m): sorted [v4 +30m, v1 +1h, v2 +2h, v3 +3h]
//	                         asOf +2h30m matches {v4, v1, v2} -> selects v2
//
// So a reader that returns anything other than version 2 has observed a state no
// serialisation of Put produces. It cannot be excused as "it read before the
// write" or "it read after the write" — there is no before or after in which
// that is the answer.
//
// The registry clock stands BEFORE every effective_at, so retainSelectable's
// first == -1 arm keeps all four versions resident. That is the estate-real
// shape it exists to preserve: one mandate in force plus the changes an operator
// has SCHEDULED and not yet reached.
func concurrencyFixture(t *testing.T) (reg *MandateRegistry, asOf time.Time, hingeLate, hingeEarly *compliancepb.Mandate) {
	t.Helper()
	base := t0
	reg = NewMandateRegistry()
	reg.now = func() time.Time { return base.Add(-time.Hour) }
	for _, m := range []*compliancepb.Mandate{
		versioned(1, base.Add(1*time.Hour)),
		versioned(2, base.Add(2*time.Hour)),
		versioned(3, base.Add(3*time.Hour)),
	} {
		mustPut(t, reg, m)
	}
	hingeLate = versioned(hingeVersion, base.Add(9*time.Hour))
	hingeEarly = versioned(hingeVersion, base.Add(30*time.Minute))
	mustPut(t, reg, hingeLate)
	return reg, base.Add(2*time.Hour + 30*time.Minute), hingeLate, hingeEarly
}

// wantVersion is the version every read in these tests must return: 2, under
// both states of the hinge.
const wantVersion = uint64(2)

// readUntil hammers Mandate from n goroutines until done closes, and returns the
// first answer that was not wantVersion plus how many reads disagreed in total.
func readUntil(reg *MandateRegistry, asOf time.Time, readers int, done <-chan struct{}) (firstBad string, bad, reads int) {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	ctx := context.Background()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for {
				select {
				case <-done:
					mu.Lock()
					reads += local
					mu.Unlock()
					return
				default:
				}
				m, gov, err := reg.Mandate(ctx, "t1", "p1", asOf)
				local++
				if err == nil && gov == Governed && m.GetVersion() == wantVersion {
					continue
				}
				mu.Lock()
				bad++
				if firstBad == "" {
					firstBad = fmt.Sprintf("read returned version=%d governance=%v err=%v",
						m.GetVersion(), gov, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstBad, bad, reads
}

// TestMandateRegistry_ConcurrentPutDoesNotChangeTheVersionSelected is #1048's
// primary proof, and it is the test the package did not have: one goroutine
// republishing while another resolves.
//
// Pre-fix it fails without the race detector, because the failure is not only a
// race: Mandate captured the version slice under the read lock, RELEASED it, and
// then walked the backing array, while Put mutated that same array in place —
// an indexed assignment on the idempotent-replace path and then sort.Slice
// permuting it. A reader walking a transiently unsorted
// array breaks the one rule its selection rests on, "versions are ascending, so
// the last match wins", and returns a version that is not in force. The gate
// then admits or refuses the order against the wrong limits and stamps that
// version into the audit fact as the authority — a trail that is internally
// consistent and wrong.
func TestMandateRegistry_ConcurrentPutDoesNotChangeTheVersionSelected(t *testing.T) {
	reg, asOf, hingeLate, hingeEarly := concurrencyFixture(t)

	// Sanity: quiescent, the fixture resolves to the invariant answer. If this
	// fails the fixture is wrong and every result below is meaningless.
	if got := atVersion(t, reg, asOf); got != wantVersion {
		t.Fatalf("quiescent resolution selected version %d, want %d — the fixture's dates are wrong", got, wantVersion)
	}

	const republishes = 60_000
	done := make(chan struct{})
	var putErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < republishes; i++ {
			m := hingeLate
			if i%2 == 1 {
				m = hingeEarly
			}
			if err := reg.Put(m); err != nil {
				putErr = err
				return
			}
		}
	}()

	firstBad, bad, reads := readUntil(reg, asOf, 3, done)
	wg.Wait()

	if putErr != nil {
		t.Fatalf("registry refused a well-formed republish: %v", putErr)
	}
	if bad > 0 {
		t.Fatalf("%d of %d concurrent resolutions returned a version no serialisation of Put produces "+
			"(first: %s). Both states of the hinge select version %d at this asOf, so this reader saw the "+
			"version array MID-MUTATION: the pre-trade gate would evaluate the order against a mandate that "+
			"is not in force and stamp that version into ComplianceResult.mandate_version as the authority "+
			"that permitted it (#1048)", bad, reads, firstBad, wantVersion)
	}
	if reads == 0 {
		t.Fatal("no resolutions ran — the reader goroutines never observed the writer, so this test proves nothing")
	}
	t.Logf("%d concurrent resolutions against %d republishes, all selecting version %d", reads, republishes, wantVersion)
}

// TestMandateRegistry_MandateSelectsUnderTheReadLock pins the READER's half of
// the fix on its own, independently of how Put is written.
//
// It has to hand-roll the writer, and that is deliberate rather than a shortcut:
// Put is now copy-on-write, so it no longer produces the transient this asserts
// against. The invariant being pinned is not "Put happens to be safe" but the
// stronger, durable one — ANY writer holding r.mu may reorder the guarded array,
// and no reader may observe that reorder. If Mandate releases the read lock
// before its selection loop, a reader can, and does. That is what makes this
// test survive a future refactor of Put back to an in-place write.
//
// The permutation used is exactly the shape sort.Slice produces: two elements of
// the matching set swapped, so "the last match wins" names the earlier version.
func TestMandateRegistry_MandateSelectsUnderTheReadLock(t *testing.T) {
	reg, asOf, _, _ := concurrencyFixture(t)
	k := mandateKey{tenant: "t1", portfolio: "p1"}

	const permutations = 20_000
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < permutations; i++ {
			reg.mu.Lock()
			vers := reg.byKey[k]
			// Swap the two versions in force at asOf and put them back, under the
			// write lock throughout — a writer doing nothing a sort would not do.
			// The yield between them widens the transient to something a reader can
			// land in: sort.Slice's own window is nanoseconds, and a test that has to
			// win a nanosecond race to detect a defect is a flaky test rather than a
			// proof. It changes only the ODDS of observing the transient, never
			// whether observing it is possible — a reader holding the read lock
			// cannot be inside this critical section for any width of it.
			vers[0], vers[1] = vers[1], vers[0]
			runtime.Gosched()
			vers[0], vers[1] = vers[1], vers[0]
			reg.mu.Unlock()
		}
	}()

	firstBad, bad, reads := readUntil(reg, asOf, 3, done)
	wg.Wait()

	if bad > 0 {
		t.Fatalf("%d of %d resolutions selected the wrong version while a writer held the WRITE lock "+
			"(first: %s). Mandate released the read lock before its selection loop, so mutual exclusion "+
			"bought nothing: the reader walked the array the writer was reordering (#1048)", bad, reads, firstBad)
	}
	if reads == 0 {
		t.Fatal("no resolutions ran — the reader goroutines never observed the writer, so this test proves nothing")
	}
	t.Logf("%d resolutions against %d write-locked permutations, all selecting version %d", reads, permutations, wantVersion)
}

// TestMandateRegistry_PutDoesNotMutateAPublishedVersionArray pins the WRITER's
// half on its own, deterministically and without goroutines.
//
// Put used to mutate the array it had already published — an indexed assignment
// and sort.Slice on the slice read straight out of byKey — and retainSelectable
// hands that same array back on its first <= 0 arm, so the array a reader
// escaped with was routinely the array the next Put reordered.
// Copy-on-write is what makes a reader that DID escape with a slice header hold
// a stable, frozen array rather than a moving one.
func TestMandateRegistry_PutDoesNotMutateAPublishedVersionArray(t *testing.T) {
	reg, _, _, hingeEarly := concurrencyFixture(t)
	k := mandateKey{tenant: "t1", portfolio: "p1"}

	reg.mu.RLock()
	held := reg.byKey[k]
	reg.mu.RUnlock()

	if len(held) != 4 {
		t.Fatalf("the fixture published %d versions, want 4 — retainSelectable pruned the scheduled "+
			"changes and there is no array left to reorder", len(held))
	}
	type snap struct {
		version uint64
		eff     time.Time
	}
	before := make([]snap, len(held))
	for i, m := range held {
		before[i] = snap{version: m.GetVersion(), eff: m.GetEffectiveAt().AsTime()}
	}

	// A republish that MOVES the hinge from last to first: the sort must permute
	// the whole array, which is precisely what a reader holding it cannot survive.
	mustPut(t, reg, hingeEarly)

	for i, m := range held {
		if m.GetVersion() != before[i].version || !m.GetEffectiveAt().AsTime().Equal(before[i].eff) {
			t.Fatalf("Put reordered an array it had already published: index %d held version %d effective %s "+
				"before the republish and version %d effective %s after. A reader that escaped with this "+
				"slice header is now walking a sequence that is not sorted, and 'the last match wins' "+
				"selects a mandate that is not in force (#1048)",
				i, before[i].version, before[i].eff, m.GetVersion(), m.GetEffectiveAt().AsTime())
		}
	}

	// And the registry itself must still resolve correctly from the NEW array.
	if got := atVersion(t, reg, t0.Add(2*time.Hour+30*time.Minute)); got != wantVersion {
		t.Fatalf("after the republish the registry selects version %d, want %d — the copy was published "+
			"unsorted or the wrong slice was published", got, wantVersion)
	}
}
