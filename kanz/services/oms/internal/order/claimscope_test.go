package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// ONE DELIVERY, ONE BUDGET (#801).
//
// The defect these cover: defaultClaimWait bounds ONE acquisition, and the
// cancel of a scheduled parent takes 1+N of them plus N venue calls on a single
// delivery. Nothing bounded the total, and bus.Subscribe puts no deadline on a
// handler's context, so the handler ran past the broker's AckWait — at which
// point the same cancel is redelivered onto a sibling pod whose lock table is
// empty and every child's venue withdrawal is dispatched a second time.

// THE SECOND CLAIM OF A DELIVERY SPENDS WHAT THE FIRST ONE LEFT.
//
// This is the arithmetic of the whole issue in one test. Two acquisitions, each
// contended for longer than the budget has left. Before the fix each one got a
// FRESH claimWait, so two claims cost 2 × claimWait and twelve cost twelve; the
// delivery's own cost was unbounded in N. After it, the second acquisition
// inherits the deadline armed for the delivery and fails on THAT, so the total
// is bounded no matter how many children a parent has.
func TestADeliverySpendsOneBudgetAcrossEveryClaim(t *testing.T) {
	const (
		budget    = 200 * time.Millisecond
		claimWait = 5 * time.Second // far longer than the budget, as in production
	)
	svc := &Service{claimWait: claimWait, deliveryBudget: budget}

	// Both orders are held by somebody else for the whole test.
	for _, id := range []string{"first", "second"} {
		release, ok := svc.claim(id)
		if !ok {
			t.Fatalf("precondition: could not hold %s", id)
		}
		defer release()
	}

	ctx, done := svc.withDeliveryDeadline(context.Background())
	defer done()

	started := time.Now()
	if _, _, err := svc.awaitClaim(ctx, "first"); err == nil {
		t.Fatal("the first claim acquired an order somebody else holds")
	}
	// The second claim is the one that matters: by now the delivery has already
	// spent its budget, and a fresh per-claim wait would let it spend another.
	if _, _, err := svc.awaitClaim(ctx, "second"); err == nil {
		t.Fatal("the second claim acquired an order somebody else holds")
	}
	elapsed := time.Since(started)

	if elapsed >= 2*budget {
		t.Fatalf("two contended claims on one delivery took %s, want under %s — each acquisition "+
			"is getting its own %s wait instead of sharing the delivery's budget, so a cancel of a "+
			"parent with N children costs N × claimWait and runs past the broker's AckWait",
			elapsed.Round(time.Millisecond), 2*budget, claimWait)
	}
}

// THE DELIVERY BUDGET IS ARMED ONCE, NOT ONCE PER CHILD.
//
// handleCancel is re-entered for every child of a scheduled parent. If arming
// were unconditional, each child would restart the 45s — restoring the exact
// per-call budget this change removes, in the place it does the most damage.
func TestArmingTheBudgetInsideADeliveryDoesNotExtendIt(t *testing.T) {
	svc := &Service{claimWait: time.Second, deliveryBudget: 300 * time.Millisecond}

	outer, done := svc.withDeliveryDeadline(context.Background())
	defer done()
	first, ok := outer.Deadline()
	if !ok {
		t.Fatal("arming the budget set no deadline — the delivery is unbounded")
	}

	// Inside a claim, which is what a child cancel re-enters holding.
	held, err := enterClaim(outer, "parent")
	if err != nil {
		t.Fatalf("enterClaim: %v", err)
	}
	inner, done2 := svc.withDeliveryDeadline(held)
	defer done2()
	second, ok := inner.Deadline()
	if !ok {
		t.Fatal("the re-entry dropped the deadline entirely")
	}
	if !second.Equal(first) {
		t.Fatalf("re-arming inside a delivery moved the deadline from %s to %s — every child "+
			"would restart the budget and the fan-out would be unbounded again",
			first, second)
	}
}

// A RE-ENTRANT CLAIM IS REFUSED AT THE DOOR, NOT BY TIMING OUT.
//
// The per-order lock is a 1-capacity channel and is not re-entrant, so a
// delivery that reaches its own order would block on a lock IT holds until the
// budget expired, then report a contention timeout naming a contender that does
// not exist. An operator would go looking for a hung venue. This is the case the
// old "ONE LOCK AT A TIME" comment asserted could not happen, three files away
// from the rule that made it true.
func TestAReentrantClaimIsRefusedImmediatelyAndByName(t *testing.T) {
	svc := &Service{claimWait: 5 * time.Second, deliveryBudget: 5 * time.Second}

	ctx, done := svc.withDeliveryDeadline(context.Background())
	defer done()

	held, release, err := svc.awaitClaim(ctx, "o-1")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer release()

	started := time.Now()
	_, _, err = svc.awaitClaim(held, "o-1")
	elapsed := time.Since(started)

	if !errors.Is(err, errReentrantClaim) {
		t.Fatalf("re-entrant claim err = %v, want errReentrantClaim — a delivery blocking on its "+
			"own lock is reported as somebody else's contention", err)
	}
	if elapsed > time.Second {
		t.Fatalf("the re-entrant claim took %s to be refused, want immediate — it is waiting out "+
			"a lock nobody will ever release", elapsed.Round(time.Millisecond))
	}
}

// CLAIMS DO NOT NEST DEEPER THAN THE PARENT/CHILD CANCEL.
//
// Depth two is the legitimate maximum: a scheduled parent's cancel withdrawing
// one of its children. A third level means validateSchedule has started
// admitting a child that is itself a parent, at which point the fan-out is no
// longer linear and the budget sized for it no longer bounds anything. The rule
// lives in another file; the consequence lands here, so the refusal does too.
func TestClaimsCannotNestPastTheParentChildCancel(t *testing.T) {
	svc := &Service{claimWait: time.Second, deliveryBudget: time.Second}

	ctx, done := svc.withDeliveryDeadline(context.Background())
	defer done()

	parent, release1, err := svc.awaitClaim(ctx, "parent")
	if err != nil {
		t.Fatalf("parent claim: %v", err)
	}
	defer release1()

	child, release2, err := svc.awaitClaim(parent, "child")
	if err != nil {
		t.Fatalf("child claim (depth 2 is legitimate): %v", err)
	}
	defer release2()

	if _, _, err := svc.awaitClaim(child, "grandchild"); !errors.Is(err, errClaimTooDeep) {
		t.Fatalf("depth-3 claim err = %v, want errClaimTooDeep", err)
	}
}

// A DELIVERY THAT WAS NEVER ARMED IS STILL BOUNDED.
//
// An unset budget must not be the one shape that silently restores the unbounded
// delivery. "Nothing configured" and "checked, and fine" must never look the same.
func TestAServiceWithNoBudgetWiredStillGetsADeadline(t *testing.T) {
	svc := &Service{} // no options, no NewService

	ctx, done := svc.withDeliveryDeadline(context.Background())
	defer done()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a Service with no budget wired armed no deadline — the delivery is unbounded, " +
			"which is the defect")
	}
	if left := time.Until(deadline); left > deliveryBudget {
		t.Fatalf("the fallback budget is %s, want at most the %s derived from the broker's AckWait",
			left, deliveryBudget)
	}
}

// ===== THE FAN-OUT, END TO END =====

// restingCloser is a venue that RESTS every order and takes a measurable amount
// of time to withdraw one.
//
// IT MODELS THE COST THE ISSUE IS ABOUT. A parent's cancel makes one closeAtVenue
// call per child, serially, on one delivery — "N closeAtVenue exchange calls
// serially in one delivery, on the single dispatch goroutine". Latency at the
// venue is therefore what multiplies by N, and it is deterministic in a way lock
// contention is not: MemoryStore.ListByParent ranges over a Go map, so the ORDER
// the children are withdrawn in changes on every call. A fixture whose cost
// depends on that order is a coin flip, not a test.
//
// Execute returns no fills, so a child is ROUTED and stays live — there is
// something left to withdraw. It is deliberately NOT SelfHealing, so the OMS
// tracks the close itself and closeAtVenue takes its full path.
type restingCloser struct {
	delay time.Duration
	mu    sync.Mutex
	// dispatched is every order CancelOrder was CALLED for; confirmed is the
	// subset the venue answered. They differ when the delivery's deadline cuts a
	// call off mid-flight, which is the case the close registry and the healing
	// watchdog exist to cover — and keeping them apart is what lets a test tell
	// "withdrawn twice" from "withdrawn once, unconfirmed".
	dispatched []string
	confirmed  []string
}

func (v *restingCloser) MIC() string     { return "XSIM" }
func (v *restingCloser) Account() string { return "acct-test" }

func (v *restingCloser) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil
}

func (v *restingCloser) CancelOrder(ctx context.Context, st *orderpb.OrderState) error {
	v.mu.Lock()
	v.dispatched = append(v.dispatched, st.GetOrderId())
	v.mu.Unlock()

	select {
	case <-time.After(v.delay):
	case <-ctx.Done():
		// A withdrawal cut off by the delivery's deadline is exactly what
		// closeAtVenue's "left to the healing watchdog" path is for: the close
		// was TRACKED before dispatch, so the watchdog resolves it against venue
		// truth. Report the error so nothing records a confirmation the venue
		// never gave.
		return ctx.Err()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.confirmed = append(v.confirmed, st.GetOrderId())
	return nil
}

// dispatchCount is how many withdrawals REACHED the venue. This is the number
// #801's second clause is about: a duplicate here is a second CancelOrder for an
// order the exchange may already have closed.
func (v *restingCloser) dispatchCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.dispatched)
}

// uniqueDispatches is dispatchCount with repeats collapsed, so the two together
// say whether any order was withdrawn twice.
func (v *restingCloser) uniqueDispatches() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range v.dispatched {
		seen[id] = true
	}
	return len(seen)
}

func (v *restingCloser) confirmedCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.confirmed)
}

// scheduledContentionService wires a Service whose children rest at a venue that
// takes venueDelay to withdraw one, with both budgets lowered so a whole
// AckWait's worth of behaviour fits inside a test.
func scheduledContentionService(t *testing.T, fb *fakeBus, at *time.Time,
	budget, venueDelay time.Duration) (*Service, *MemoryStore, *restingCloser, *execution.CloseRegistry) {
	t.Helper()
	store := NewMemoryStore()
	venue := &restingCloser{delay: venueDelay}
	closes := execution.NewCloseRegistry()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), closes, nil,
		WithHaltGate(halt.OpenGate(nil)),
		WithClaimWait(50*time.Millisecond),
		WithDeliveryBudget(budget))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return *at }
	return svc, store, venue, closes
}

// twelveLiveChildren admits a 12-slice parent and drives every slice, so the
// cancel below has the fan-out #801's Verified-when names.
func twelveLiveChildren(t *testing.T, svc *Service, store *MemoryStore) []string {
	t.Helper()
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 12, nil))); err != nil {
		t.Fatalf("admit the parent: %v", err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	var live []string
	for _, c := range children {
		if !IsTerminal(c) {
			live = append(live, c.GetOrderId())
		}
	}
	if len(live) != 12 {
		t.Fatalf("precondition: %d live children, want 12 — the fan-out this issue is about "+
			"does not exist in this fixture", len(live))
	}
	return live
}

func cancelParent(t *testing.T, svc *Service) error {
	t.Helper()
	return svc.Handle(testCtx(), &envelopepb.Envelope{EventType: SubjectCancel},
		mustMarshal(t, &orderpb.CancelOrder{
			Metadata: &commandpb.CommandMetadata{
				TargetId:            "p1",
				PrincipalPortfolios: []string{"fund-alpha"},
			},
			OrderId: "p1",
		}))
}

// A CANCEL OF A SLOW-TO-WITHDRAW SCHEDULED PARENT STOPS INSIDE ITS BUDGET.
//
// #801's Verified-when, the negative half. Twelve children at a venue that takes
// 120ms to withdraw one: the fan-out's own cost is 12 x 120ms = 1.44s, serial, on
// one delivery, and before the fix NOTHING bounded it — bus.Subscribe puts no
// deadline on a handler's context and claimWait bounds only one acquisition. At
// production numbers that is what walks a cancel past a 60s AckWait, and past
// AckWait the broker redelivers it onto a sibling pod whose lock table is empty
// and which re-dispatches a venue withdrawal for every child.
//
// After the fix the delivery stops when ITS budget is gone. Stopping is the
// correct outcome and not a degraded one: nothing is announced, the parent stays
// live, the children already withdrawn are terminal, and the replay skips them.
func TestScheduleE2E_ACancelOfASlowParentStopsInsideItsBudget(t *testing.T) {
	const (
		budget     = 600 * time.Millisecond
		venueDelay = 120 * time.Millisecond
	)
	at := schedEnd
	fb := &fakeBus{}
	svc, store, venue, closes := scheduledContentionService(t, fb, &at, budget, venueDelay)
	live := twelveLiveChildren(t, svc, store)

	started := time.Now()
	err := cancelParent(t, svc)
	elapsed := time.Since(started)

	// STOPPING MUST BE HONEST. An error nacks, so the command parks in the DLQ
	// where an operator can see and replay it.
	if err == nil {
		t.Fatal("the cancel reported success without withdrawing every child — the operator has " +
			"been told the order was pulled while slices of it are still live at a venue")
	}

	// THE WHOLE FAN-OUT WOULD COST len(live) x venueDelay. The budget is what has
	// to bound it. The assertion is deliberately loose: this is proving the
	// difference between "bounded by the delivery" and "bounded by nothing", not
	// measuring a scheduler.
	if unbounded := time.Duration(len(live)) * venueDelay; elapsed >= unbounded {
		t.Fatalf("the cancel took %s, which is the whole %s the serial venue withdrawals cost — "+
			"the delivery is bounded by children x venue latency rather than by one budget, so "+
			"at production numbers it runs past the broker's AckWait and the cancel is "+
			"redelivered while this copy is still working",
			elapsed.Round(time.Millisecond), unbounded)
	}
	if elapsed >= 3*budget {
		t.Fatalf("the cancel took %s against a %s budget, want it to stop inside a small "+
			"multiple of its own budget", elapsed.Round(time.Millisecond), budget)
	}

	// AND THE PARENT IS NOT MARKED CANCELLED. A terminal parent is skipped by
	// resume() and the sweep forever after, so a premature one strands every
	// child that was never withdrawn.
	parent, _, lerr := store.Load(context.Background(), "p1")
	if lerr != nil {
		t.Fatalf("Load parent: %v", lerr)
	}
	if parent.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("the parent was saved CANCELLED even though the withdrawal stopped early")
	}

	// NON-VACUITY, BOTH WAYS. The fan-out must have started — otherwise this
	// passes just as well against a cancel refused at the parent — and it must
	// NOT have finished, or the budget bound nothing and the timing assertion
	// above is decoration.
	withdrawn := terminalChildren(t, store, live)
	if withdrawn == 0 {
		t.Fatal("no child was withdrawn at all — the cancel never reached the fan-out, so this " +
			"test is not measuring what it claims to")
	}
	if withdrawn == len(live) {
		t.Fatal("every child was withdrawn, so the budget never bound anything")
	}
	if venue.dispatchCount() == 0 {
		t.Fatal("no venue withdrawal was dispatched — the fixture is not exercising closeAtVenue")
	}

	// NOTHING THE LEDGER CALLED CANCELLED WENT TO THE VENUE UNRECORDED. A
	// withdrawal the deadline cut off mid-flight is unconfirmed, not lost: it was
	// Tracked BEFORE dispatch, so it is still in the close registry for the
	// healing watchdog to resolve against venue truth. That path is pre-existing
	// — closeAtVenue reports no error precisely so the ledger never freezes on a
	// venue that will not answer — but the delivery deadline is a NEW way to
	// reach it, so it is asserted here rather than assumed.
	unconfirmed := venue.dispatchCount() - venue.confirmedCount()
	if unconfirmed > 0 && closes.Len() < unconfirmed {
		t.Fatalf("%d venue withdrawals went unconfirmed but only %d are tracked for healing — a "+
			"cancel the exchange may never have received has been dropped, and the ledger calls "+
			"that order CANCELLED", unconfirmed, closes.Len())
	}
}

// THE SAME TWELVE-CHILD CANCEL COMPLETES WHEN THE VENUE IS ORDINARILY FAST.
//
// #801's Verified-when, the positive half, and the assertion that stops the fix
// from being a regression. The budget must bound the pathological case WITHOUT
// turning the ordinary one into a DLQ entry: a parent whose children withdraw at
// a normal venue latency must still be pulled whole, in one delivery.
func TestScheduleE2E_ACancelOfATwelveChildParentCompletesInsideItsBudget(t *testing.T) {
	const (
		budget     = 3 * time.Second
		venueDelay = 2 * time.Millisecond
	)
	at := schedEnd
	fb := &fakeBus{}
	svc, store, venue, closes := scheduledContentionService(t, fb, &at, budget, venueDelay)
	live := twelveLiveChildren(t, svc, store)

	started := time.Now()
	err := cancelParent(t, svc)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("cancelling a parent whose twelve children withdraw normally failed: %v — the "+
			"budget is refusing ordinary work, and an operator's withdrawal is in the DLQ", err)
	}
	if elapsed >= budget {
		t.Fatalf("the cancel took %s against a %s budget", elapsed.Round(time.Millisecond), budget)
	}

	// EVERY child is withdrawn, at the ledger AND at the venue, and the parent is
	// terminal. Anything less is the failure cancelChildren exists to prevent: a
	// pulled order with slices still working at an exchange.
	if got := terminalChildren(t, store, live); got != len(live) {
		t.Errorf("%d of %d children are terminal after their parent was cancelled", got, len(live))
	}
	if got := venue.dispatchCount(); got != len(live) {
		t.Errorf("%d venue withdrawals dispatched for %d live children", got, len(live))
	}
	if got := venue.confirmedCount(); got != len(live) {
		t.Errorf("%d of %d venue withdrawals were confirmed", got, len(live))
	}
	if closes.Len() != 0 {
		t.Errorf("%d closes are still awaiting healing after every withdrawal was confirmed",
			closes.Len())
	}
	parent, _, lerr := store.Load(context.Background(), "p1")
	if lerr != nil {
		t.Fatalf("Load parent: %v", lerr)
	}
	if parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("the parent is %s after a successful cancel, want CANCELLED", parent.GetStatus())
	}
}

// A REPLAY OF AN ABANDONED CANCEL ISSUES NO SECOND VENUE WITHDRAWAL FOR A CHILD
// IT ALREADY WITHDREW.
//
// #801's Verified-when, second clause. Abandoning is only safe if resuming is
// cheap: closeAtVenue is "not idempotent from the exchange's point of view", so a
// replay that re-dispatched every child would be the duplicate venue traffic the
// budget exists to prevent, arriving through the door the budget opened.
//
// cancelChildren skips a terminal child BEFORE it builds a command for it, so the
// replay's venue calls are exactly the children the first delivery never reached —
// and a third delivery, with the parent now CANCELLED and announced, reaches the
// venue not at all.
func TestScheduleE2E_ReplayingAnAbandonedCancelDoesNotReWithdrawAChild(t *testing.T) {
	const (
		budget     = 600 * time.Millisecond
		venueDelay = 120 * time.Millisecond
	)
	at := schedEnd
	fb := &fakeBus{}
	svc, store, venue, closes := scheduledContentionService(t, fb, &at, budget, venueDelay)
	live := twelveLiveChildren(t, svc, store)

	if err := cancelParent(t, svc); err == nil {
		t.Fatal("the first delivery withdrew all twelve children inside its budget — this test " +
			"needs a delivery that stops early to have anything to replay")
	}
	firstPass := terminalChildren(t, store, live)
	if firstPass == 0 || firstPass == len(live) {
		t.Fatalf("the first delivery withdrew %d of %d children; this test needs a partial "+
			"withdrawal", firstPass, len(live))
	}
	// EVERY CHILD THE LEDGER MARKED TERMINAL REACHED THE VENUE. Confirmations can
	// be fewer — the deadline can cut a call off mid-flight, and that close stays
	// tracked for the watchdog — but a cancellation recorded with no dispatch at
	// all would be a withdrawal that only ever happened in the ledger.
	if got := venue.dispatchCount(); got < firstPass {
		t.Fatalf("%d venue withdrawals for %d children marked terminal — the ledger recorded a "+
			"cancellation it never sent to the exchange", got, firstPass)
	}

	// THE REPLAY. Same command, a delivery that can afford the rest of the fan-out.
	svc.deliveryBudget = 5 * time.Second
	if err := cancelParent(t, svc); err != nil {
		t.Fatalf("the replay of an abandoned cancel failed: %v", err)
	}
	if got := terminalChildren(t, store, live); got != len(live) {
		t.Fatalf("%d of %d children are terminal after the replay", got, len(live))
	}
	parent, _, lerr := store.Load(context.Background(), "p1")
	if lerr != nil {
		t.Fatalf("Load parent: %v", lerr)
	}
	if parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("the parent is %s after the replay, want CANCELLED", parent.GetStatus())
	}

	// ONE WITHDRAWAL PER CHILD ACROSS BOTH DELIVERIES. This is the assertion the
	// issue's second clause names: the replay must not re-dispatch a cancel for a
	// child the first delivery already pulled off the exchange.
	if got, uniq := venue.dispatchCount(), venue.uniqueDispatches(); got != len(live) || uniq != got {
		t.Fatalf("%d venue withdrawals across %d distinct children for %d live children over two "+
			"deliveries — the replay re-cancelled an order at the exchange, and closeAtVenue is "+
			"not idempotent from the exchange's point of view", got, uniq, len(live))
	}

	// A THIRD DELIVERY REACHES THE VENUE NOT AT ALL: every child is terminal and
	// the parent carries cancel_announced_at, so the already-cancelled branch
	// answers without calling closeAtVenue.
	// AN UNCONFIRMED WITHDRAWAL IS TRACKED, NOT LOST. The replay does not re-issue
	// it — the child is terminal, so cancelChildren skips it — which is only safe
	// because the close registry still holds it for the healing watchdog.
	if unconfirmed := venue.dispatchCount() - venue.confirmedCount(); unconfirmed > closes.Len() {
		t.Fatalf("%d withdrawals went unconfirmed but only %d are tracked for healing — the "+
			"replay will not re-issue them, so an order the exchange may still be resting is "+
			"CANCELLED in the ledger with nothing left to resolve it", unconfirmed, closes.Len())
	}

	before := venue.dispatchCount()
	if err := cancelParent(t, svc); err != nil {
		t.Fatalf("a third delivery of the same cancel failed: %v", err)
	}
	if got := venue.dispatchCount(); got != before {
		t.Fatalf("a third delivery dispatched %d more venue withdrawals, want 0", got-before)
	}
}

// A FAN-OUT THAT RUNS OUT OF BUDGET SAYS SO, AND SAYS HOW FAR IT GOT.
//
// The abandoned cancel is a nack and a DLQ entry either way, so the message is
// the only thing distinguishing "this cancel spent its share of AckWait after
// withdrawing 5 of 12 slices, and a replay resumes from there" from "a venue is
// hung on child 6". Those want opposite responses from whoever is on call, and
// an operator holding a half-withdrawn parent on the exit path needs to know
// which one they have without reading the store.
//
// awaitClaim reports a spent budget too, but it cannot report the COUNT — it does
// not know it is inside a fan-out. That is what this checkpoint adds, and it is
// why the check sits at the child boundary rather than being left to the claim.
func TestScheduleE2E_AnAbandonedFanOutReportsHowFarItGot(t *testing.T) {
	const (
		budget     = 600 * time.Millisecond
		venueDelay = 120 * time.Millisecond
	)
	at := schedEnd
	fb := &fakeBus{}
	svc, store, _, _ := scheduledContentionService(t, fb, &at, budget, venueDelay)
	live := twelveLiveChildren(t, svc, store)

	err := cancelParent(t, svc)
	if err == nil {
		t.Fatal("the cancel completed inside its budget — nothing was abandoned to report on")
	}
	if !errors.Is(err, errBudgetSpent) {
		t.Fatalf("an abandoned fan-out reported %v, want errBudgetSpent — a cancel that ran out "+
			"of its share of AckWait is indistinguishable from a hung venue in the DLQ", err)
	}

	withdrawn := terminalChildren(t, store, live)
	if withdrawn == 0 || withdrawn == len(live) {
		t.Fatalf("the fan-out withdrew %d of %d children; this test needs a partial one",
			withdrawn, len(live))
	}
	// THE COUNT IN THE MESSAGE MUST BE THE COUNT IN THE STORE. A report that
	// disagrees with the ledger is worse than no report: it sends an operator
	// looking for slices that were never withdrawn, or leaves ones that were.
	if want := fmt.Sprintf("%d of %d children were withdrawn", withdrawn, len(live)); !strings.Contains(err.Error(), want) {
		t.Fatalf("the abandoned cancel reported %q, which does not contain %q — the operator is "+
			"being told a different number from the one in the ledger", err, want)
	}
}

// terminalChildren counts how many of ids are terminal in the store.
func terminalChildren(t *testing.T, store *MemoryStore, ids []string) int {
	t.Helper()
	n := 0
	for _, id := range ids {
		c, _, err := store.Load(context.Background(), id)
		if err != nil {
			t.Fatalf("Load child %s: %v", id, err)
		}
		if IsTerminal(c) {
			n++
		}
	}
	return n
}

// The child ids the schedule derives must be the ones the fixture holds; if
// ChildID ever stopped agreeing with what the driver writes, every contention
// test above would hold locks nobody contends and pass vacuously.
func TestTwelveChildFixtureHoldsTheIdsTheDriverWrote(t *testing.T) {
	at := schedEnd
	fb := &fakeBus{}
	svc, store, _, _ := scheduledContentionService(t, fb, &at, time.Second, time.Millisecond)
	live := twelveLiveChildren(t, svc, store)

	want := make(map[string]bool, len(live))
	for i := 0; i < len(live); i++ {
		want[schedule.ChildID("p1", i)] = true
	}
	for _, id := range live {
		if !want[id] {
			t.Fatalf("the driver wrote child %s, which schedule.ChildID does not derive — the "+
				"contention fixtures are holding locks nothing else takes", id)
		}
	}
	if len(want) != len(live) {
		t.Fatalf("%d derived ids for %d children", len(want), len(live))
	}
}
