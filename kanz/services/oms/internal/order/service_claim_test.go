package order

import (
	"sync"
	"testing"
)

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
