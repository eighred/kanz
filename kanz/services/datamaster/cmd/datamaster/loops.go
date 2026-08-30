package main

import (
	"context"
	"sync"
)

// joinThenClose is datamaster's shutdown ordering, in one named place (#815).
//
// # WHY IT IS A FUNCTION AND NOT THREE LINES IN A DEFER
//
// Same reason armProposalPurge is one: the decision lives at the composition
// root, no unit test reaches a `defer func(){…}()`, and this repository has
// shipped crashes through exactly that blind spot. The three statements below
// are the entire fix for #815 and their ORDER is the whole of it, so the order
// is asserted rather than described — see loops_test.go.
//
// # WHAT GOES WRONG WITHOUT THE WAIT
//
// closeStores() is pool.Close(). Three goroutines drive that pool: the outbox
// relay, the golden projector (which holds the per-tenant cycle lock) and the
// lapsed-proposal purge. main() is os.Exit(run()), so when run() returns those
// goroutines do not wind down — they stop existing, wherever they are.
//
// For the relay that is not merely a late FACT. It publishes to the broker and
// THEN marks the record published, which is the correct order (a mark that
// preceded the publish would lose the FACT outright on a crash), and it holds a
// cluster-wide per-key advisory lock across both. Measured against Postgres in
// internal/outbox's TestPostgresOutboxARelayLeftRunningIsStillHoldingTheDrainLock:
// at the moment the owning frame's teardown begins, the relay is still inside
// the drain, still holding that lock, with the FACT already at the broker and
// the record still marked pending. Ending the process there releases the lock by
// killing the session and leaves a record the next process republishes.
//
// # ONE PREMISE THAT DOES NOT HOLD, CHECKED RATHER THAN INHERITED
//
// services/oms/cmd/oms/main.go justifies the same join by saying that lock
// "would be released by a connection teardown rather than by its own unlock",
// naming pool.Close() as the mechanism. Measured on pgx v5, that mechanism is
// wrong: pool.Close() BLOCKS while a connection is checked out and the held
// connection keeps working, so a relay inside a locked drain does get to run its
// own unlock. (What Close refuses is a NEW acquisition — "closed pool", even on
// an uncancelled context.)
//
// The conclusion survives and the mechanism is worse than the one recorded:
// nothing blocks os.Exit, and Relay's unlock runs on context.Background
// precisely so that a cancelled shutdown still releases the drain lock. An
// unjoined relay is the one case where that care buys nothing.
func joinThenClose(cancel context.CancelFunc, loops *sync.WaitGroup, closeStores func()) {
	// 1. Tell the loops to stop. Every one of them selects on this context, so
	//    without the cancel the Wait below would never return.
	cancel()
	// 2. WAIT. This is the line #815 is about: it is what makes the close below
	//    happen after the last query rather than during one.
	loops.Wait()
	// 3. Only now is the pool nobody is using any more torn down.
	closeStores()
}
