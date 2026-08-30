// RUN OWNS BOTH LOOPS' LIFETIME, so a caller that joins Run has joined
// everything Run started (#813).
//
// The failure this file exists to prevent is not a leaked goroutine counter. It
// is a snapshot publish still inside the bus producer while the caller's
// deferred `client.Close()` runs:
//
//	pkg/alpha/runner.go            wg.Wait()  — waits on Engine.Run and nothing else
//	services/market-ingest/…/main  wg.Wait(); then LIFO defers close the bus client
//
// Engine.Run used to return on the fold loop's first send, leaving snapshotLoop
// holding a live ticker outside every WaitGroup. Nothing between here and the
// process exit waited for it, so the publish and the Close raced — and the loser
// is a market-data snapshot that either errors at the least observed moment of
// the deployment or is lost with a single Warn line, because publishSnapshot
// logs its error and returns.
package ingest

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

// gatedPublisher parks inside Publish so a test can hold a snapshot publish
// open across the moment Run returns — the exact window in which the caller's
// deferred client.Close() would fire.
type gatedPublisher struct {
	entered chan struct{} // closed when the first Publish is in flight
	release chan struct{} // closed by the test to let that Publish return

	mu sync.Mutex
	// afterOwnerLeft counts Publish calls that were still running, or that
	// started, after the owner declared the transport torn down.
	afterOwnerLeft int
	ownerLeft      bool

	once sync.Once
}

func newGatedPublisher() *gatedPublisher {
	return &gatedPublisher{entered: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedPublisher) Publish(context.Context, bus.Event) error {
	p.note()
	p.once.Do(func() { close(p.entered) })
	<-p.release
	p.note()
	return nil
}

func (p *gatedPublisher) note() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ownerLeft {
		p.afterOwnerLeft++
	}
}

// ownerHasLeft stands in for the caller's `defer client.Close()`: from this
// point on, any publish is a publish against a transport that is going away.
func (p *gatedPublisher) ownerHasLeft() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ownerLeft = true
}

func (p *gatedPublisher) publishesAfterOwnerLeft() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.afterOwnerLeft
}

// all is the count-stable read of capture's events: last() cannot distinguish
// "nothing further published" from "the same snapshot published again".
func (c *capture) all() []bus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Event(nil), c.events...)
}

// foldThenFailSource emits one book snapshot so the book is non-empty (an empty
// book publishes nothing), then blocks until the test releases a fatal error.
// That hands the test control of the instant the fold loop returns.
type foldThenFailSource struct {
	first bool
	fail  chan struct{}
}

func (s *foldThenFailSource) Recv(ctx context.Context) (depth.Update, error) {
	if !s.first {
		s.first = true
		return depth.Update{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 1,
			Bids: []*marketpb.PriceLevel{lvl("50000", "1")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "1")},
		}}, nil
	}
	select {
	case <-s.fail:
		return depth.Update{}, io.ErrUnexpectedEOF
	case <-ctx.Done():
		return depth.Update{}, ctx.Err()
	}
}

// A FATAL SOURCE ERROR MUST NOT LET RUN RETURN OVER A LIVE PUBLISH.
//
// This is the issue's own "Verified when", made deterministic: the snapshot loop
// is parked inside Publish when the fold loop dies, so if Run returns while that
// publish is in flight the goroutine has outlived the function that started it —
// and pkg/alpha's wg.Wait() and market-ingest's client.Close() are both on the
// other side of that return.
func TestRunDoesNotReturnWhileASnapshotPublishIsInFlight(t *testing.T) {
	pub := newGatedPublisher()
	src := &foldThenFailSource{fail: make(chan struct{})}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: pub,
		SnapshotInterval: 5 * time.Millisecond, SnapshotDepth: 10, Tenant: "acme",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()

	// The snapshot loop is now inside publishSnapshot and cannot leave.
	select {
	case <-pub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot publish started — the ticker or the fold never ran, so this test " +
			"would prove nothing about the shutdown race it exists for")
	}

	// Kill the fold loop. Run's error is now available.
	close(src.fail)

	select {
	case err := <-runErr:
		// Run returned with the snapshot loop still parked in Publish. Every
		// caller's join has now completed over a live publish.
		pub.ownerHasLeft()
		close(pub.release)
		t.Fatalf("Run returned (err=%v) while a snapshot publish was still in flight. "+
			"snapshotLoop is outside Run's join, so pkg/alpha's wg.Wait() completes and "+
			"market-ingest's deferred client.Close() runs underneath it", err)
	case <-time.After(250 * time.Millisecond):
		// Correct: Run is waiting for the snapshot loop.
	}

	close(pub.release)

	select {
	case err := <-runErr:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Run err = %v, want ErrUnexpectedEOF — the fatal source error must still surface", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned after the publish completed — the join must be released by " +
			"cancelling the loops, not only by the caller cancelling ctx")
	}

	// Whatever ran after Run returned would have run against a closing client.
	pub.ownerHasLeft()
	time.Sleep(50 * time.Millisecond)
	if n := pub.publishesAfterOwnerLeft(); n != 0 {
		t.Fatalf("%d snapshot publish(es) ran after Run returned — the snapshot loop is still "+
			"alive past its owner", n)
	}
}

// RUN MUST STOP THE SNAPSHOT LOOP ITSELF, not lean on the caller's cancel.
//
// Today market-ingest's cancel() rescues this by accident: the rescue lives in
// the caller, so Run's own doc ("until ctx is cancelled") holds only for callers
// that remembered. A caller whose ctx outlives the engine — every caller that
// runs one engine per feed off a shared context, which is pkg/alpha exactly —
// gets a ticker that keeps publishing after the engine it belongs to is done.
func TestRunStopsTheSnapshotLoopWithoutTheCallerCancelling(t *testing.T) {
	pub := &capture{}
	src := &foldThenFailSource{fail: make(chan struct{})}
	close(src.fail) // fail on the second Recv, immediately

	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: pub,
		SnapshotInterval: 5 * time.Millisecond, SnapshotDepth: 10, Tenant: "acme",
	})

	// NOT cancelled, and not on a timeout: the caller's context stays live for
	// the whole test, so anything that stops is stopped by Run.
	if err := eng.Run(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Run err = %v, want ErrUnexpectedEOF", err)
	}

	before := len(pub.all())
	time.Sleep(20 * 5 * time.Millisecond) // ~20 ticker periods
	if after := len(pub.all()); after != before {
		t.Fatalf("%d further snapshot(s) published after Run returned (%d → %d). The ticker "+
			"outlives the engine on any caller whose context is not cancelled the moment Run "+
			"returns — and the caller is not where this lifetime belongs", after-before, before, after)
	}
}
