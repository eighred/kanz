package execution

import (
	"context"
	"testing"
	"time"
)

// THE DELAY GROWS, CAPS, AND ONLY Reset UNDOES IT (#1047).
//
// The policy is shared by both connectors and by both of their failure paths, so
// a defect here is a defect on every venue at once. What it must never do is
// return to the base delay on its own: the loop that used to reset on a
// successful connect is exactly what let an unreadable order view drive an
// unbounded re-dial loop against a healthy exchange.
func TestUserDataBackoffGrowsAndCapsAndResetsOnlyOnDemand(t *testing.T) {
	// A cancelled context makes Wait return immediately, so the schedule can be
	// exercised without sleeping through it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := NewUserDataBackoff()
	if got := b.Delay(); got != UserDataBackoffBase {
		t.Fatalf("first delay = %v, want the base %v", got, UserDataBackoffBase)
	}

	var seen []time.Duration
	for i := 0; i < 10; i++ {
		seen = append(seen, b.Delay())
		b.Wait(ctx)
	}

	// It DOUBLES until the cap, and never shrinks on its own.
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("delay went backwards: %v then %v (schedule %v) — a back-off that shrinks "+
				"without being reset re-opens the re-dial loop it exists to close", seen[i-1], seen[i], seen)
		}
	}
	if got := b.Delay(); got != UserDataBackoffMax {
		t.Errorf("after 10 failed sessions the delay is %v, want the cap %v (schedule %v)",
			got, UserDataBackoffMax, seen)
	}
	if seen[1] != 2*UserDataBackoffBase {
		t.Errorf("second delay = %v, want %v — the growth is not exponential", seen[1], 2*UserDataBackoffBase)
	}

	// Reset is the ONLY way back, and the caller owes it evidence of progress.
	b.Reset()
	if got := b.Delay(); got != UserDataBackoffBase {
		t.Errorf("after Reset the delay is %v, want the base %v", got, UserDataBackoffBase)
	}
}

// Wait RETURNS ON CANCELLATION rather than holding shutdown for up to the cap.
// A pod that takes 30 seconds to stop is a pod the orchestrator kills, and a
// killed venue adapter is one that never runs its deferred close on the venue
// connection.
func TestUserDataBackoffWaitReturnsWhenTheContextEnds(t *testing.T) {
	b := NewUserDataBackoff()
	for i := 0; i < 6; i++ { // drive it to the cap
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		b.Wait(ctx)
	}
	if b.Delay() != UserDataBackoffMax {
		t.Fatalf("delay = %v, want the cap %v before timing the wait", b.Delay(), UserDataBackoffMax)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	b.Wait(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait took %v on a cancelled context with a %v delay pending — shutdown would "+
			"block behind the back-off", elapsed, UserDataBackoffMax)
	}
}
