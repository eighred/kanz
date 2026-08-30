package main

// THE SHUTDOWN ORDERING, ASSERTED (#815).
//
// run() used to `defer closeStores()` and then start three goroutines that drive
// the pool closeStores() tears down — the outbox relay, the golden projector and
// the lapsed-proposal purge — with nothing joining any of them. main() is
// os.Exit(run()), so run() returning is not a wind-down: it is the point at
// which those goroutines cease to exist, mid-query or not.
//
// What these tests pin is the ORDER of the three statements in joinThenClose,
// because the order is the entire fix. They are deliberately not tests of a
// WaitGroup field being present: each one starts a real goroutine that touches a
// store handle AFTER its context is cancelled, which is exactly what the relay
// does (its unlock deliberately runs on context.Background so a cancelled
// shutdown still releases the drain lock), and asserts that the store was still
// open when it did.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeStores records the sequence of events around a close, so a failure can
// show WHAT happened rather than only that something did.
type fakeStores struct {
	mu     sync.Mutex
	closed bool
	// useAfterClose counts store work that ran once the pool was gone. In
	// production that is a "closed pool" error at best and a killed goroutine at
	// worst; here it is a number, so the assertion can name it.
	useAfterClose int
	events        []string
}

func (s *fakeStores) use(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.useAfterClose++
	}
	s.events = append(s.events, what)
}

func (s *fakeStores) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.events = append(s.events, "closeStores")
}

func (s *fakeStores) sequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// settleWindow is how long each fake loop keeps working after its context is
// cancelled.
//
// IT IS A MARGIN, NOT A SLEEP-AND-HOPE. The property is an ordering, and
// joinThenClose either blocks for this window or it does not — with the Wait
// removed, closeStores() runs microseconds after cancel(), so the failure is not
// a coin flip that a slow machine could hide. Making it larger only makes the
// mutation fail harder; it can never make a broken ordering pass.
const settleWindow = 150 * time.Millisecond

// TestTheStoresAreNotClosedUnderALiveLoop is the #815 assertion.
//
// The loop does what the relay does on shutdown: it notices the cancellation and
// then still has work to finish against the store. If closeStores() runs first,
// that work lands on a pool that no longer exists.
func TestTheStoresAreNotClosedUnderALiveLoop(t *testing.T) {
	stores := &fakeStores{}
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup

	finished := make(chan struct{})
	loops.Add(1)
	go func() {
		defer loops.Done()
		defer close(finished)
		<-ctx.Done()
		// The tail of a drain: the FACT is with the broker and the record still
		// has to be marked published. This is the work that must not meet a
		// closed pool.
		time.Sleep(settleWindow)
		stores.use("relay marks the record published")
	}()

	joinThenClose(cancel, &loops, stores.close)

	// The loop is joined, so this has already returned. It is here for the
	// MUTATED reading: without the Wait, joinThenClose returns first and the
	// assertions below would otherwise inspect a store the loop has not reached
	// yet — reporting "nothing happened" instead of "it happened after the
	// close", which is the finding.
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the background loop never finished")
	}

	if stores.useAfterClose != 0 {
		t.Errorf("%d store operation(s) ran AFTER closeStores(). That is #815: the outbox relay "+
			"publishes a FACT and then marks the record published, so a close landing between the "+
			"two leaves the estate holding an announcement the table still calls pending — and the "+
			"next process republishes it.\n\nsequence: %v", stores.useAfterClose, stores.sequence())
	}
	want := []string{"relay marks the record published", "closeStores"}
	if got := stores.sequence(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("teardown ran as %v, want %v — closeStores() must be the LAST thing that happens, "+
			"after every loop holding a store handle has returned", got, want)
	}
}

// TestEveryLoopIsJoinedNotJustTheFirst. run() starts three of these, and a Wait
// that somehow covered one would be worse than no Wait at all: it would make the
// shutdown look ordered while two loops still raced the close.
func TestEveryLoopIsJoinedNotJustTheFirst(t *testing.T) {
	stores := &fakeStores{}
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup

	var finished sync.WaitGroup
	for _, name := range []string{"outbox relay", "golden projector", "lapsed-proposal purge"} {
		loops.Add(1)
		finished.Add(1)
		go func() {
			defer loops.Done()
			defer finished.Done()
			<-ctx.Done()
			time.Sleep(settleWindow)
			stores.use(name)
		}()
	}

	joinThenClose(cancel, &loops, stores.close)
	finished.Wait() // already true when joined; see the note in the test above

	if stores.useAfterClose != 0 {
		t.Errorf("%d of the three background loops touched the stores after they were closed"+
			"\n\nsequence: %v", stores.useAfterClose, stores.sequence())
	}
	seq := stores.sequence()
	if len(seq) != 4 || seq[3] != "closeStores" {
		t.Errorf("teardown ran as %v — all three loops must finish before closeStores()", seq)
	}
}

// TestTheLoopsAreCancelledBeforeTheyAreWaitedOn. The cancel is load-bearing in
// the other direction: without it joinThenClose blocks forever on loops that are
// waiting to be told to stop, and a shutdown that hangs is a pod that gets
// SIGKILLed with the relay mid-pass — the failure this whole change exists to
// remove, arriving by a different door.
func TestTheLoopsAreCancelledBeforeTheyAreWaitedOn(t *testing.T) {
	stores := &fakeStores{}
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup

	loops.Add(1)
	go func() {
		defer loops.Done()
		<-ctx.Done() // returns ONLY if joinThenClose cancels
	}()

	returned := make(chan struct{})
	go func() { defer close(returned); joinThenClose(cancel, &loops, stores.close) }()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("joinThenClose never returned: it waited on loops it had not cancelled. A shutdown " +
			"that blocks is a pod SIGKILLed with the relay mid-pass, which is the same loss #815 " +
			"is about")
	}
}
