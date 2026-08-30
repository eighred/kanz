package outbox

// WHAT AN UNJOINED RELAY IS HOLDING WHEN ITS OWNER RETURNS (#815).
//
//	TEST_POSTGRES_URL=… go test -p 1 -run 'TestPostgresOutboxARelayLeftRunning' ./internal/outbox/
//
// services/oms/cmd/oms/main.go joins its relay goroutine into the WaitGroup its
// frame waits on. services/datamaster/cmd/datamaster/main.go did not, and #815
// is that gap. Every one of these composition roots is os.Exit(run()), so a
// frame returning is not a wind-down: the goroutines it never joined stop
// existing, wherever they are.
//
// This test is here rather than beside either composition root because the
// claim needs an engine. A fake queue has no connection, no session and no
// advisory lock, so it cannot exhibit any of what follows.
//
// # WHAT WAS MEASURED, AND ONE INHERITED CLAIM THAT IS FALSE
//
// The OMS comment justified its join by saying the relay's per-key advisory lock
// "would be released by a connection teardown rather than by its own unlock",
// with pool.Close() as the mechanism. Measured on pgx v5, that mechanism does
// not hold: pool.Close() BLOCKS while a connection is checked out, and the held
// connection keeps working, so a relay inside a locked drain does get to run its
// own unlock. (What Close refuses is a NEW acquisition — "closed pool", even on
// an uncancelled context.)
//
// The CONCLUSION survives; the mechanism is a different one, and it is worse.
// The lock is not lost to pool.Close, it is lost to os.Exit — nothing blocks
// THAT, and the relay's unlock is deliberately written on context.Background
// precisely so a cancelled shutdown still releases the drain lock. An unjoined
// relay is the one case where that care is spent for nothing.
//
// # WHAT THIS ASSERTS
//
//  1. the goroutine is STILL RUNNING once its owner's teardown has begun — the
//     defect observed, not inferred from a missing WaitGroup;
//  2. it is holding the cluster-wide per-key drain lock at that moment, proved
//     from an INDEPENDENT connection rather than from this process's belief;
//  3. the broker already has the FACT while the outbox table still calls the
//     record pending — the window a killed process leaves behind, and the reason
//     the next process republishes it.
//
// None of this is a race claim. -race needs cgo and does not run on the usual
// Windows box; this is an ordering and a lifetime, both forced deterministically
// by a publisher that parks until the test lets it go.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// blockingPublisher parks inside Publish until the test releases it, which is
// what holds the relay mid-drain — advisory lock taken, connection checked out —
// while the owning frame tears down around it.
type blockingPublisher struct {
	entered chan struct{}
	release chan struct{}
	sent    int
}

func (p *blockingPublisher) Publish(context.Context, bus.Event) error {
	p.entered <- struct{}{}
	<-p.release
	p.sent++
	return nil
}

func TestPostgresOutboxARelayLeftRunningIsStillHoldingTheDrainLock(t *testing.T) {
	pool := newPool(t, testTenant)
	applySchema(t)
	ctx := testCtx()
	q := NewPostgres(pool, "datamaster")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(ctx, tx, pgFact(t, ctx, "datamaster.exception.overridden", "unjoined-1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	pub := &blockingPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	var logged strings.Builder
	relay, err := NewRelay(q, pub, slog.New(slog.NewTextHandler(&logged, nil)), WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// THE DATAMASTER SHAPE BEFORE #815, COPIED: `go func(){ relay.Run(ctx) }()`
	// with nothing joining it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := relay.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("relay.Run: %v", err)
		}
	}()

	select {
	case <-pub.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never reached the publisher — the drain did not start, so this test " +
			"proves nothing about shutdown ordering")
	}

	// The owner's teardown begins: the signal context is cancelled and the
	// deferred closeStores() fires. Close runs on its own goroutine because it
	// blocks on the drain's checked-out connection — which is itself the measured
	// correction described in the header.
	cancel()
	closed := make(chan struct{})
	go func() { defer close(closed); pool.Close() }()

	// (1) THE GOROUTINE OUTLIVES ITS OWNER.
	select {
	case <-done:
		t.Fatal("the relay goroutine returned before the teardown — this test needs it to be " +
			"mid-drain, or it is not reproducing #815")
	default:
	}

	// (2) AND IT IS HOLDING THE DRAIN LOCK. Asked through the package's own
	// LockKey on a SEPARATE pool, so the answer comes from Postgres rather than
	// from this process's bookkeeping. In production the frame returns here and
	// os.Exit destroys this session: the lock is then released by the connection
	// dying, and Relay's unlock — written on context.Background so that a
	// cancelled shutdown still releases it — never runs at all.
	witness := newPool(t, testTenant)
	if release, ok, lerr := NewPostgres(witness, "datamaster").LockKey(ctx, "unjoined-1", false); lerr != nil {
		t.Fatalf("witness LockKey: %v", lerr)
	} else if ok {
		release()
		t.Error("the drain lock for unjoined-1 was FREE while the relay was inside its drain. " +
			"Either LockKey stopped taking a session lock or the relay stopped holding one across " +
			"the publish — and if so, the whole argument for joining this goroutine needs rewriting " +
			"rather than this assertion deleting")
	}

	// The broker accepts the FACT. Everything the relay does after this is store
	// work during a teardown.
	close(pub.release)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the relay goroutine never returned")
	}
	<-closed

	if pub.sent != 1 {
		t.Fatalf("the publisher accepted %d event(s), want 1 — the broker must have the FACT for "+
			"the record below to be a duplicate in waiting", pub.sent)
	}

	// (3) The record is read back on the witness pool, because the relay's own
	// pool no longer exists — which is the point.
	pending, err := NewPostgres(witness, "datamaster").Pending(ctx, "unjoined-1", 10)
	if err != nil {
		t.Fatalf("Pending on the witness pool: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the record was marked published during the teardown (%d pending). That would "+
			"mean this window does not exist — re-read the relay's MarkPublished path before "+
			"deleting anything on the strength of it", len(pending))
	}
	if !strings.Contains(logged.String(), "could not be marked") {
		t.Errorf("the relay did not report that a PUBLISHED record could not be marked. It is the "+
			"only line an operator would ever see for this, and without it the duplicate on the "+
			"next start has no explanation at all.\n\nlogged:\n%s", logged.String())
	}
	t.Logf("#815: the owner's teardown began with the relay still inside a drain, still holding "+
		"the cluster-wide lock for unjoined-1, with the FACT already at the broker and the record "+
		"still pending.\n\nrelay log:\n%s", logged.String())
}
