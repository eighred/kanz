package order

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// defaultClaimWait bounds how long a cancel or an amend will wait for the
// goroutine currently working an order to let go of it.
//
// IT IS A CEILING ON HEAD-OF-LINE BLOCKING, NOT A TUNING KNOB. work() holds the
// per-order lock across venue.Execute — a real call to an exchange — and one bus
// subject is dispatched by ONE goroutine, so while a cancel waits here, every
// OTHER order's cancel queued behind it on that subject waits too. JetStream now
// bounds how deep that queue can get: MaxAckPending is 32 on work subjects
// (pkg/bus/tuning.go), where it was previously the 1000 server default.
//
// It must also stay comfortably under JetStream's AckWait, which is 60s for
// order subjects (bus.WorkAckWait). Past AckWait the broker redelivers this same
// command while the first copy is still blocked. 5s leaves 12× headroom.
//
// THAT HEADROOM IS PER ACQUISITION, AND IT IS NO LONGER THE BINDING CONSTRAINT
// (#801). A cancel of a scheduled parent takes 1+N of these — one per child —
// plus N venue round-trips, all on one delivery, so twelve contended slices
// consumed 65s of an AckWait of 60. The bound that actually holds the delivery
// inside the broker's clock is deliveryBudget (claimscope.go), armed once per
// command and shared by every acquisition below it. This number is now what
// bounds head-of-line blocking for ONE order; that one bounds the command.
//
// WHAT THIS COMMENT USED TO SAY, AND WHY IT WAS WRONG (#237). It claimed that at
// the redelivery "the consumer's dedup window is still holding its idempotency
// key, and the redelivery is ACKED AND DISCARDED as a duplicate — a cancel
// deleted by a timeout arithmetic error". The window was NOT holding it. An
// in-flight dispatch holds only the claim LEASE; the full TTL is held only after
// Commit (pkg/bus/dedup.go). The lease was 5s against a 30s AckWait, so at the
// redelivery it had been expired for 25s and Claim returned TRUE — the opposite
// failure from the one described: not a cancel silently deleted, but a SECOND
// COPY of it dispatched concurrently with the first. The lease is now derived as
// maxTunedAckWait + margin (75s) and outlasts every AckWait, so the suppression
// this comment always assumed finally exists — but note it is the IN-PROCESS
// window that provides it. Across pods the arbiter is still Store.Save's version
// predicate (#122); see claim's doc below.
const defaultClaimWait = 5 * time.Second

// orderLocks is the per-order mutual-exclusion table: one lock per order_id,
// created on first use and deleted when the last holder or waiter lets go.
//
// WHY NOT A sync.Map OF struct{}, WHICH IS WHAT THIS REPLACES. That table was a
// try-lock and nothing else. Try is the RIGHT and COMPLETE answer for
// handleSubmit and resume — "another goroutine already owns this order, so there
// is nothing for me to do" is true there, because the holder will carry the
// order to completion on this delivery's behalf. It is the WRONG answer for a
// cancel: nobody else is going to cancel the order on its behalf, so a cancel
// that gives up is a cancel that was silently dropped — an operator pulling an
// order back off an exchange and being told nothing at all. Cancel must WAIT,
// and a set cannot be waited on, so the entry has to become a lock.
//
// IT IS STILL IN-PROCESS ONLY, and that is still a real limit rather than an
// oversight — see claim's doc and the Save note in postgres.go.
type orderLocks struct {
	mu    sync.Mutex
	locks map[string]*orderLock
}

// orderLock is one order's lock, plus the number of goroutines that hold it or
// are waiting for it. refs is guarded by orderLocks.mu; sem IS the lock — a
// 1-capacity channel rather than a sync.Mutex, because a mutex cannot be
// abandoned and the waiter here must be able to give up on a deadline.
type orderLock struct {
	sem  chan struct{} // cap 1: a send acquires, a receive releases
	refs int           // holders + waiters; at 0 the entry is deleted
}

// ref takes a reference to orderID's lock, creating it if the table has none.
// The caller MUST eventually unref exactly once, whether or not it goes on to
// acquire the lock itself.
//
// The zero orderLocks is usable: the map is created here, so a bare &Service{}
// (service_claim_test.go) keeps working.
func (t *orderLocks) ref(orderID string) *orderLock {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = make(map[string]*orderLock)
	}
	l, ok := t.locks[orderID]
	if !ok {
		l = &orderLock{sem: make(chan struct{}, 1)}
		t.locks[orderID] = l
	}
	l.refs++
	return l
}

// unref drops one reference and deletes the entry when the last one goes.
//
// WHY THE REFCOUNT EXISTS AT ALL. A plain map[string]*sync.Mutex grows by one
// entry per order_id for the life of the process, keyed by a string the outside
// world supplies — an unbounded leak in a service that will see millions of
// order ids. Deleting the entry on release WITHOUT a refcount is worse than the
// leak: a waiter blocked on a lock whose map entry has been deleted is waiting
// on a lock nobody will ever look up again, so the next arrival creates a SECOND
// lock for the same order and both goroutines run — the exact failure this table
// exists to prevent, reintroduced by the cleanup.
//
// WHY IT IS RACE-FREE. refs is only ever read or written under t.mu, and an
// entry is only ever REACHED under t.mu (ref, above). So when unref sees
// refs == 0 while holding t.mu, there is no holder, no waiter, and no goroutine
// sitting between "found it in the map" and "counted itself" — that window does
// not exist, because both happen inside one critical section. The entry is
// unreachable, and deleting it is safe. A goroutine arriving afterwards builds a
// fresh lock, which is correct: nobody held the old one.
func (t *orderLocks) unref(orderID string, l *orderLock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l.refs--
	if l.refs > 0 {
		return
	}
	// Identity, not just the key: while any reference is outstanding the entry
	// cannot have been deleted or replaced, so cur == l always holds. The check
	// is here so that if that invariant is ever broken by a future change, this
	// deletes nothing rather than evicting a live lock somebody is holding.
	if cur, ok := t.locks[orderID]; ok && cur == l {
		delete(t.locks, orderID)
	}
}

// releaser returns the release function handed to a caller that acquired the
// lock. It is idempotent: a double release would decrement refs twice and could
// delete an entry another goroutine still holds, so the sync.Once is not
// decoration.
//
// It frees the lock BEFORE dropping the reference, so a waiter can take it
// immediately and that waiter's own reference keeps the entry alive across the
// handover.
func (t *orderLocks) releaser(orderID string, l *orderLock) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.sem
			t.unref(orderID, l)
		})
	}
}

// claim takes exclusive, in-process ownership of one order_id WITHOUT WAITING,
// and returns the function that releases it. ok is false when another goroutine
// in THIS process already holds it, and the caller must then do nothing at all.
//
// TRY IS THE CORRECT SEMANTIC AT ITS TWO CALL SITES and must stay that way.
// handleSubmit and resume are both asking "should I drive this order?", and
// "somebody already is" is a complete answer: the holder will carry it to
// completion, so backing off loses nothing. A cancel asks a different question
// and gets a different primitive — see awaitClaim.
//
// WHAT THIS IS FOR, AND WHAT IT IS NOT. store.Create is the ADMISSION gate: it
// decides which of two concurrent first-deliveries owns a NEW order. It has
// nothing to say about two goroutines that both find an order that ALREADY
// exists — a redelivery re-driving an order to a venue, or a cancel rewriting
// one mid-execution.
//
// It is per-order, not global: a single lock would serialize every order in the
// OMS behind the slowest venue call.
//
// It is IN-PROCESS ONLY, and that is a boundary, not a gap. What this closes is
// the window WITHIN a pod, which is the window the bus actually opens: submit,
// amend and cancel are three separate durables with three cursors and three
// dispatch goroutines (see Subscribe's durable-per-(group,subject) comment in
// pkg/bus/nats.go), concurrent by construction — and the consumer dedup claim
// does not close it, because those are three DIFFERENT events with three
// different idempotency keys.
//
// ACROSS PODS, Store.Save's version predicate closes it (#122): two OMS pods
// acting on one order are arbitrated by the engine, and the losing writer is
// REJECTED with ErrConflict rather than silently overwriting the winner. This
// comment used to say that predicate was still needed. It exists now — see the
// Save comment in postgres.go — so the two mechanisms together cover both
// windows, and neither is a substitute for the other: the lock keeps one pod's
// goroutines from interleaving mid-execution, and the version keeps two pods
// from discarding each other's transitions.
func (s *Service) claim(orderID string) (func(), bool) {
	l := s.working.ref(orderID)
	select {
	case l.sem <- struct{}{}:
		return s.working.releaser(orderID, l), true
	default:
		// Held. Drop the reference we took to look, so probing an order nobody
		// is working leaves nothing behind in the table.
		s.working.unref(orderID, l)
		return func() {}, false
	}
}

// awaitClaim takes THE SAME per-order lock claim takes, but waits for it.
//
// It exists for cancel and amend, where neither of claim's two outcomes is
// acceptable: proceeding without the lock acts on state the holder is about to
// overwrite (a CANCELLED order Saved back to FILLED), and returning because the
// lock is held drops an operator's cancel on the floor. The only correct answer
// is to wait, then look again.
//
// THE WAIT IS BOUNDED ON PURPOSE. work() holds this lock across venue.Execute,
// and closeAtVenue makes a venue call under it too, so the holder can be as slow
// as an exchange is. An unbounded wait here would let one hung venue stall the
// cancel subject's whole dispatch goroutine indefinitely — every order's cancel,
// not just this one's. On expiry the caller gets an error and NOTHING has been
// decided about the command; see the call sites for what they do with it.
//
// ctx is honoured too, so a shutdown does not have to wait out the timeout.
//
// LOCKS NEST, ONE LEVEL, PARENT BEFORE CHILD — AND THAT IS ENFORCED HERE (#801).
//
// WHAT THIS COMMENT USED TO SAY, AND WHY IT WAS WRONG. It claimed "ONE LOCK AT A
// TIME IS AN INVARIANT OF THIS PACKAGE. No path acquires a second order lock
// while holding one". That stopped being true when a cancel learned to withdraw
// a scheduled parent's children: handleCancel takes the parent's lock and then,
// still holding it, runs cancelChildren, which re-enters handleCancel per child
// and takes the child's. The comment was dated evidence for a property the code
// no longer had, in the file the next person reasoning about lock ordering would
// read first — which is the compounding hazard, not the nesting itself.
//
// THE REAL INVARIANT, AND WHY IT CANNOT DEADLOCK. Acquisition is ordered parent
// then child, and the order is acyclic because a child can never itself be a
// parent — validateSchedule refuses a command carrying both a parent and a
// schedule. So the deepest a delivery goes is two, and no two deliveries can
// hold each other's next lock.
//
// IT IS ENFORCED RATHER THAN ASSERTED. That acyclicity rests on a rule in
// another file, and the cost of relaxing it is not a bug report — it is this
// dispatch goroutine blocking on a lock it holds itself. enterClaim (claimscope.go)
// therefore refuses a re-entrant claim at the door, instantly and by name,
// and refuses a third level outright. t.mu remains a leaf never held across a
// channel operation.
//
// THE WAIT SPENDS THE DELIVERY'S BUDGET, NOT A FRESH ONE. claimWait bounds this
// acquisition; the deadline already on ctx bounds the whole delivery, and
// context.WithTimeout keeps whichever is earlier. That is what stops 1+N
// acquisitions from costing 1+N × claimWait on one AckWait clock.
//
// THE RETURNED CONTEXT MUST BE USED for everything done while holding the lock,
// including a nested cancel — it is what carries the claim record that makes the
// two refusals above possible.
func (s *Service) awaitClaim(ctx context.Context, orderID string) (context.Context, func(), error) {
	held, err := enterClaim(ctx, orderID)
	if err != nil {
		return nil, nil, err
	}

	wait := s.claimWait
	if wait <= 0 {
		wait = defaultClaimWait
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	l := s.working.ref(orderID)
	select {
	case l.sem <- struct{}{}:
		return held, s.working.releaser(orderID, l), nil
	case <-waitCtx.Done():
		s.working.unref(orderID, l)
		// WHICH CLOCK RAN OUT IS THE WHOLE DIAGNOSIS. ctx.Err() non-nil means the
		// DELIVERY's budget is gone — this cancel has already spent its share of
		// AckWait on earlier children, and the operator should be looking at the
		// fan-out, not at this order. A bare timeout means this ONE order is held
		// too long, which is a hung venue. They read the same in a log unless the
		// message says which.
		if cerr := ctx.Err(); cerr != nil {
			// s.deliveryBudget, NOT the package constant: a test lowers it, and a
			// message that always names 45s while the delivery actually had 600ms
			// tells an operator the wrong thing about the only number that matters
			// here.
			return nil, nil, fmt.Errorf(
				"%w (%s) while waiting for the goroutine working order %s to release it — this "+
					"command has spent its whole share of the broker's AckWait, so it stops "+
					"here rather than running past it and being redelivered while this copy is "+
					"still working: %w",
				errBudgetSpent, s.budget(), orderID, cerr)
		}
		return nil, nil, fmt.Errorf(
			"oms: gave up after %s waiting for the goroutine working order %s to release it: %w",
			wait, orderID, waitCtx.Err())
	}
}
