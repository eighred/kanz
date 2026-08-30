package order

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// waiters reports how many goroutines hold or are waiting for orderID's lock.
// It reaches into the lock table on purpose: a fix that leaks one entry per
// order_id is a memory leak keyed by a string the outside world supplies, and
// nothing observable from outside the package would catch it.
func (s *Service) waiters(orderID string) int {
	s.working.mu.Lock()
	defer s.working.mu.Unlock()
	if l := s.working.locks[orderID]; l != nil {
		return l.refs
	}
	return 0
}

// tableSize reports how many order locks the table is currently holding.
func (s *Service) tableSize() int {
	s.working.mu.Lock()
	defer s.working.mu.Unlock()
	return len(s.working.locks)
}

// Two goroutines that both decide to resume one order must not both resume it.
// store.Create guards ADMISSION — the first delivery — and says nothing about
// two deliveries that both find an order already there. After the resume path
// exists, "both proceed" means both re-drive to the venue.
func TestClaimAdmitsExactlyOneHolderPerOrder(t *testing.T) {
	svc := &Service{}

	const goroutines = 64
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		held  int
	)
	start.Add(1)
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			release, ok := svc.claim("o-1")
			if !ok {
				return
			}
			defer release()
			mu.Lock()
			held++
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()

	if held == 0 {
		t.Fatal("no goroutine obtained the claim — the order would never be worked at all")
	}
	if held > goroutines {
		t.Fatalf("impossible: %d holders from %d goroutines", held, goroutines)
	}
	// The claim must be re-obtainable after release, or one crashed resume
	// poisons the order for the process lifetime.
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("claim not available after every holder released — a released claim " +
			"that stays held freezes the order until the pod restarts")
	}
	release()
}

// Distinct orders must never block each other. A single global lock here would
// serialize the entire OMS behind the slowest venue call.
func TestClaimIsPerOrderNotGlobal(t *testing.T) {
	svc := &Service{}
	releaseA, ok := svc.claim("order-a")
	if !ok {
		t.Fatal("first claim on order-a failed")
	}
	defer releaseA()

	releaseB, ok := svc.claim("order-b")
	if !ok {
		t.Fatal("order-b was blocked by a claim on order-a — the claim is global, " +
			"which serializes every order in the OMS behind one venue call")
	}
	releaseB()
}

// Holding a claim must exclude a second claim on the SAME order.
func TestClaimExcludesTheSameOrder(t *testing.T) {
	svc := &Service{}
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("first claim failed")
	}
	if _, ok := svc.claim("o-1"); ok {
		release()
		t.Fatal("a second claim on a held order succeeded — two goroutines would " +
			"both resume it, and both would re-drive it to the venue")
	}
	release()
}

// A cancel must WAIT for the goroutine working the order, not give up. claim's
// try semantics are right for submit and resume, where the holder finishes the
// job on the loser's behalf; nobody finishes a cancel on a delivery's behalf, so
// backing off there means an operator's withdrawal was silently discarded.
func TestAwaitClaimBlocksUntilTheHolderReleases(t *testing.T) {
	svc := &Service{}
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("first claim failed")
	}

	acquired := make(chan func(), 1)
	failed := make(chan error, 1)
	go func() {
		_, r, err := svc.awaitClaim(context.Background(), "o-1")
		if err != nil {
			failed <- err
			return
		}
		acquired <- r
	}()

	// It must still be waiting while the holder holds.
	select {
	case <-acquired:
		t.Fatal("awaitClaim returned while another goroutine held the lock — a cancel " +
			"would act on state the holder is about to overwrite, which is the whole defect")
	case err := <-failed:
		t.Fatalf("awaitClaim gave up instead of waiting: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case r := <-acquired:
		r()
	case err := <-failed:
		t.Fatalf("awaitClaim never acquired after the holder released: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("awaitClaim did not acquire after the holder released — a released lock " +
			"that stays held freezes every cancel for that order until the pod restarts")
	}

	if got := svc.tableSize(); got != 0 {
		t.Errorf("lock table holds %d entries after everyone released, want 0", got)
	}
}

// The wait is bounded: work() holds the lock across a real call to an exchange,
// and one bus subject is dispatched by one goroutine, so an unbounded wait would
// let a single hung venue stall EVERY order's cancel.
func TestAwaitClaimGivesUpWhenItsContextExpires(t *testing.T) {
	svc := &Service{}
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("first claim failed")
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, _, err := svc.awaitClaim(ctx, "o-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("awaitClaim err = %v, want a wrapped context.DeadlineExceeded", err)
	}

	// The abandoned waiter must have dropped its reference — only the holder
	// should be counted. A stranded refcount would keep the entry alive forever.
	if got := svc.waiters("o-1"); got != 1 {
		t.Errorf("refs = %d after the waiter gave up, want 1 (the holder alone) — "+
			"a waiter that abandons its reference leaks the entry", got)
	}
}

// claim and awaitClaim must be two doors onto ONE lock. If they ever ended up
// with separate tables, a cancel and the goroutine working the order would both
// believe they held exclusive ownership.
func TestAwaitClaimAndClaimShareOneLock(t *testing.T) {
	svc := &Service{}

	_, held, err := svc.awaitClaim(context.Background(), "o-1")
	if err != nil {
		t.Fatalf("awaitClaim on a free order: %v", err)
	}
	if _, ok := svc.claim("o-1"); ok {
		t.Fatal("claim succeeded on an order awaitClaim holds — they are not the same lock")
	}
	held()

	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("claim failed after awaitClaim released")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := svc.awaitClaim(ctx, "o-1"); err == nil {
		t.Fatal("awaitClaim acquired an order claim holds — they are not the same lock")
	}
	release()
}

// The table is keyed by an order_id the outside world supplies, so it must not
// grow. Under -race this also proves try and blocking acquisition contending on
// one entry never hand the lock to two goroutines at once.
func TestOrderLockTableDoesNotGrow(t *testing.T) {
	svc := &Service{}

	for i := 0; i < 1000; i++ {
		release, ok := svc.claim(fmt.Sprintf("o-%d", i))
		if !ok {
			t.Fatalf("claim on fresh order o-%d failed", i)
		}
		release()
	}
	if got := svc.tableSize(); got != 0 {
		t.Fatalf("lock table holds %d entries after 1000 claim/release cycles, want 0 — "+
			"one entry per order_id for the life of the process is an unbounded leak", got)
	}

	const goroutines = 64
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		live  int
		maxIn int
	)
	enter := func() {
		mu.Lock()
		defer mu.Unlock()
		live++
		if live > maxIn {
			maxIn = live
		}
	}
	leave := func() {
		mu.Lock()
		defer mu.Unlock()
		live--
	}

	start.Add(1)
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			if i%2 == 0 {
				release, ok := svc.claim("contended")
				if !ok {
					return
				}
				enter()
				leave()
				release()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, release, err := svc.awaitClaim(ctx, "contended")
			if err != nil {
				return
			}
			enter()
			leave()
			release()
		}(i)
	}
	start.Done()
	done.Wait()

	if maxIn > 1 {
		t.Fatalf("%d goroutines were inside the lock at once, want 1 — try and blocking "+
			"acquisition are not excluding each other", maxIn)
	}
	if got := svc.tableSize(); got != 0 {
		t.Fatalf("lock table holds %d entries after the contended run, want 0", got)
	}
}
