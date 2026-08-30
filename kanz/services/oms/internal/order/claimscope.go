package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// THE BUDGET IS PER DELIVERY, NOT PER CLAIM (#801).
//
// # The defect this exists to remove
//
// defaultClaimWait bounds ONE acquisition. For a very long time that was also
// the bound on a whole command, because every handler took exactly one claim.
// The cancel of a WORKING_SCHEDULED parent broke that assumption without
// changing the number: handleCancel takes the parent's claim and then, still
// holding it, calls cancelChildren, which runs the ordinary cancel path once per
// live child — one awaitClaim and one closeAtVenue venue round-trip apiece, all
// on the single dispatch goroutine for order.order.cancel.
//
// So the cost of one delivery is 1+N claims plus N exchange calls while the
// number that was supposed to bound it stayed 5s. Twelve contended slices is
// 65s of waiting alone, against a 60s AckWait — and nothing anywhere stopped it,
// because bus.Subscribe puts NO deadline on the context it hands a handler
// (pkg/bus/nats.go). The handler simply ran past the broker's clock.
//
// # Why running past AckWait is a money defect and not a latency one
//
// At AckWait the broker redelivers a command whose first copy is STILL RUNNING.
// The OMS wires no cross-pod deduper, so at replicas: 2 the redelivery lands on
// the sibling pod, whose lock table is empty, and it re-dispatches closeAtVenue
// for every child — a call this package's own comment says is "not idempotent
// from the exchange's point of view". Duplicate withdrawal traffic to a live
// exchange, and meanwhile every other order's cancel is queued behind the first
// copy on the EXIT path, which is the one path that must never jam: a cancel
// that cannot get through is a position that cannot be closed.
//
// # The fix is one deadline for the whole delivery
//
// A budget derived from AckWait is armed once, at the outermost entry into the
// cancel path, and carried in the context. Every claim below it, and every venue
// call below it, spends the SAME budget, because context.WithTimeout keeps the
// earlier of the two deadlines. The handler therefore always returns — abandoning
// honestly and parking in the DLQ if it has to — BEFORE the broker would decide
// the pod died. The redelivery it used to manufacture cannot happen, so the
// duplicate venue call cannot happen either.
//
// Abandoning is safe and is the designed outcome: nothing is announced, the
// parent stays WORKING_SCHEDULED, and any children already withdrawn are
// terminal, so a replay from the DLQ skips them (cancelChildren's IsTerminal
// check) and issues no second venue call for them.
//
// deliveryBudget is one delivery's share of the broker's AckWait, and the
// FORMULA now lives in exactly one place (#836).
//
// #801 added this budget here because nothing else bounded a handler. That was
// the right fix in the wrong layer: bus.Subscribe now bounds EVERY delivery in
// the estate from the consumer's own AckWait, so the arithmetic that used to be
// spelled out in this file — an AckWait, a margin, a subtraction — is gone, and
// this is the same number read from bus's own fraction rather than a second copy
// that could drift away from it.
//
// IT IS STILL NEEDED, AND THE REASON IS NOT THE BUS PATH. Every command delivery
// arrives already bounded, so withDeliveryDeadline does nothing for those. What
// it still covers is the entry points that are NOT bus deliveries: DriveSchedules
// runs off a ticker in the composition root and reaches awaitClaim through
// retireIfFinished, with no broker anywhere in the call stack and therefore no
// deadline unless this file supplies one. Deleting this outright — which is what
// #836 originally proposed, before that path was traced — would have left the
// scheduler's claims unbounded while the command path got safer.
//
// One formula, two entry points, is not the duplication #836 exists to remove.
// Two formulas would have been.
const deliveryBudget = bus.WorkAckWait * bus.HandlerBudgetNumerator / bus.HandlerBudgetDenominator

// THE ORDERING IS ASSERTED BY THE COMPILER, not by this paragraph. If a future
// change drives bus.HandlerBudget to zero or below — a non-positive AckWait, a
// fraction inverted — every armed delivery expires instantly and every cancel of
// a scheduled parent goes to the DLQ. That is a total outage of the exit path,
// arrived at by editing a number in another package, and it must not be
// discoverable only in production. The conversion of a negative constant to an
// unsigned type does not compile.
//
// This is the same idiom pkg/bus/dedup.go uses to bind the dedup claim lease to
// maxTunedAckWait. It is uint64 rather than that file's uint because these
// operands are nanosecond durations — 45e9 does not fit in a 32-bit uint, so on
// a 32-bit target the uint form would fail to compile for a reason that has
// nothing to do with the ordering it is asserting.
const _ = uint64(deliveryBudget - 1)

// maxClaimDepth is how many per-order claims one delivery may hold at once.
//
// TWO, AND THE SECOND ONE IS THE PARENT/CHILD CANCEL. handleCancel holds the
// parent's claim across cancelChildren, which takes each child's claim in turn —
// so the cancel path is genuinely nested, one level deep, and the "ONE LOCK AT A
// TIME" claim orderlock.go used to make was simply false.
//
// It terminates at one level because validateSchedule refuses a command carrying
// both a parent and a schedule, so a child can never itself be a parent. THAT IS
// A RULE THREE FILES AWAY, and the cost of it being relaxed is not a bug report —
// it is the cancel subject's dispatch goroutine deadlocking against itself, or a
// fan-out that is quadratic in a budget sized for linear. So the depth it implies
// is enforced HERE, at the acquisition, where the consequence lands.
const maxClaimDepth = 2

// scopeKey is the context key for a delivery's claim scope. Unexported empty
// struct: nothing outside this package can plant or read one.
type scopeKey struct{}

// claimScope is the set of order claims one delivery is currently holding.
//
// IT IS AN IMMUTABLE LINKED LIST, NOT A MAP, and that is deliberate. A mutable
// value in a context is shared by every derived context, so a release deep in
// the call tree would mutate a scope its caller still relies on; and it would be
// read and written by whatever goroutines a future handler spawns, with no lock,
// which is a data race that -race cannot see on this box (no cgo). A cons cell
// per claim is allocation-cheap at depth two, and the "release" is simply the
// caller going back to using the context it already had.
type claimScope struct {
	parent  *claimScope
	orderID string
	depth   int
}

// errReentrantClaim is returned when a delivery tries to take a claim it is
// already holding. Named, so the call sites and the tests can tell it apart from
// a genuine contention timeout: the two want opposite operator responses.
var errReentrantClaim = errors.New("oms: re-entrant per-order claim")

// errClaimTooDeep is returned when a delivery tries to nest claims deeper than
// the cancel path's parent/child fan-out.
var errClaimTooDeep = errors.New("oms: per-order claims nested too deeply")

// withDeliveryDeadline arms the one budget that bounds a whole command delivery.
//
// IT ARMS AT MOST ONCE PER DELIVERY. handleCancel is re-entered for each child of
// a scheduled parent, and a second arming there would hand the child a FRESH
// 45s — restoring exactly the per-call budget this change exists to remove, in
// the one place it does the most damage. The marker that says "already armed" is
// the scope itself: a delivery that holds any claim is by definition already
// inside an armed handler.
//
// A context that already carries an earlier deadline keeps it: context.WithTimeout
// takes the earlier of the two, so a shutdown or a caller-imposed bound is never
// widened by arming.
func (s *Service) withDeliveryDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if scopeOf(ctx) != nil {
		return ctx, func() {}
	}
	// ALREADY BOUNDED IS ALREADY DONE (#836). Every bus delivery arrives with a
	// deadline derived from its consumer's AckWait, so on the command path this
	// returns immediately and the broker's number is the one in force — there is
	// no second, local budget stacked underneath it that could disagree. What
	// falls through to the arming below is the ticker path, which has no broker.
	if _, bounded := ctx.Deadline(); bounded {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.budget())
}

// budget is the delivery budget this Service actually runs with.
//
// A bare &Service{} — service_claim_test.go builds one — has none wired. It falls
// back to the constant rather than to "no deadline at all": an unset budget must
// not be the one shape that silently restores the unbounded delivery this file
// exists to remove. "Nothing configured" and "checked, and fine" must never look
// the same.
//
// EVERY MESSAGE THAT NAMES THE BUDGET READS IT FROM HERE, so a delivery that ran
// with a lowered budget never reports the production constant instead.
func (s *Service) budget() time.Duration {
	if s.deliveryBudget > 0 {
		return s.deliveryBudget
	}
	return deliveryBudget
}

// scopeOf returns the claim scope this context carries, or nil.
func scopeOf(ctx context.Context) *claimScope {
	s, _ := ctx.Value(scopeKey{}).(*claimScope)
	return s
}

// enterClaim validates that orderID may be claimed by this delivery and returns
// the context the claim's holder must use for everything it does while holding
// it.
//
// THE RETURNED CONTEXT IS NOT OPTIONAL. A caller that keeps using the context it
// passed in loses the record of what it holds, and the next claim below it —
// the child cancel — is then indistinguishable from a fresh delivery: no
// re-entrancy check, no depth bound. That is why awaitClaim returns a context
// rather than mutating one in place, and why the arch guard in
// test/arch/one_cancel_delivery_one_budget_test.go refuses a re-rooted context
// anywhere in this package.
func enterClaim(ctx context.Context, orderID string) (context.Context, error) {
	cur := scopeOf(ctx)
	for s := cur; s != nil; s = s.parent {
		if s.orderID == orderID {
			// SELF-DEADLOCK, CAUGHT AT THE DOOR. The per-order lock is not
			// re-entrant — sem is a 1-capacity channel — so without this the
			// delivery would block on a lock it holds itself until the budget
			// expired, then park in the DLQ with a contention error naming a
			// contender that does not exist. An operator would go looking for a
			// hung venue. Say what actually happened instead.
			return nil, fmt.Errorf(
				"%w: this delivery already holds order %s, so waiting for it would block on "+
					"its own lock until the delivery budget expired and then report a "+
					"contention timeout naming a contender that does not exist. A cancel "+
					"reached itself, which means an order is its own ancestor",
				errReentrantClaim, orderID)
		}
	}
	depth := 1
	if cur != nil {
		depth = cur.depth + 1
	}
	if depth > maxClaimDepth {
		return nil, fmt.Errorf(
			"%w: order %s would be claim number %d in one delivery, and the deepest legitimate "+
				"nesting is %d (a scheduled parent's cancel withdrawing one of its children). "+
				"A third level means validateSchedule is admitting a child that is itself a "+
				"parent, and the fan-out is no longer bounded by the budget sized for it",
			errClaimTooDeep, orderID, depth, maxClaimDepth)
	}
	return context.WithValue(ctx, scopeKey{}, &claimScope{
		parent:  cur,
		orderID: orderID,
		depth:   depth,
	}), nil
}

// budgetLeft reports how much of the delivery budget remains, and whether a
// budget is armed at all. An unarmed context (a unit test calling a handler
// directly, DriveSchedules' own ticker) reports false and is never fenced.
func budgetLeft(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}

// errBudgetSpent is what a fan-out returns when it stops because the delivery
// budget is gone rather than because anything failed.
//
// IT IS A DISTINCT OUTCOME AND MUST READ AS ONE. "This cancel ran out of its
// share of AckWait after withdrawing 5 of 12 slices, and the replay resumes from
// there" and "a venue is hung on child 6" are the same nack and the same DLQ
// entry, and they want opposite responses from the operator on call.
var errBudgetSpent = errors.New("oms: the delivery budget is spent")

// affordsAnotherClaim reports whether enough of the delivery budget is left to
// start another order's withdrawal, and how much is left.
//
// WHAT IT GUARANTEES, EXACTLY: no child's withdrawal is STARTED without enough
// budget left to acquire that child's lock. The reserve is the single-claim wait
// because that is the first thing the withdrawal does and the smallest thing it
// cannot proceed without — if the budget cannot cover the lock, nothing after
// the lock can start either.
//
// WHAT IT DOES NOT GUARANTEE, so that nobody later relies on it for this: it
// does NOT reserve the venue round-trip or the Save. Those are unbounded from
// here — a venue is as slow as an exchange is — so the deadline can still land
// inside a withdrawal this check waved through. That case is covered, and was
// before this change: closeAtVenue Tracks the close BEFORE dispatching it, so a
// call cut off mid-flight is an in-flight close the healing watchdog resolves
// against venue truth, not a lost one.
//
// WHY IT IS WORTH HAVING ANYWAY. It stops the delivery at a CHILD BOUNDARY, with
// a count of what was withdrawn, instead of inside an acquisition that had no
// chance of succeeding — and the count is the thing an operator needs and that
// no error raised further down can supply.
func (s *Service) affordsAnotherClaim(ctx context.Context) (time.Duration, bool) {
	left, armed := budgetLeft(ctx)
	if !armed {
		return 0, true
	}
	reserve := s.claimWait
	if reserve <= 0 {
		reserve = defaultClaimWait
	}
	return left, left >= reserve
}

// commitGrace is how long a write that RECORDS AN EFFECT ALREADY AT THE EXCHANGE
// may run after the delivery budget is gone.
//
// It is spent out of the quarter of AckWait the budget deliberately leaves
// unspent (bus.HandlerBudget), not out of the budget itself, so the handler still
// returns inside AckWait: 45s of budget plus at most one 5s grace is 50s against
// 60s, leaving the DLQ publish the rest. At most ONE grace period is
// reachable per delivery — once the budget is spent, affordsAnotherClaim stops
// the fan-out before the next child rather than letting each one overrun.
const commitGrace = 5 * time.Second

// THE LEDGER MUST RECORD WHAT THE VENUE WAS TOLD (#801).
//
// # The hole this closes, which was MEASURED and not reasoned about
//
// closeAtVenue dispatches a real CancelOrder to the exchange and the Save that
// records the cancellation comes after it. Both were running on the delivery's
// context. Against MemoryStore that is invisible — it ignores the context and
// commits anyway — but pgx does not, and the OMS runs on pgx. With the budget
// expiring between the two, the exchange had been told to pull a slice and the
// ledger had no record of it, so the parent nacked, the replay found the child
// still live, and dispatched a SECOND CancelOrder.
//
// TestPostgres_AnAbandonedFanOutWithdrawsEachChildExactlyOnce measured exactly
// that: 13 venue withdrawals for 12 children. Which is the duplicate cancel
// traffic the budget was added to prevent, arriving through the door the budget
// opened — a fix that moved the defect rather than removing it.
//
// # Why the write, specifically, is allowed to outlive the budget
//
// The budget exists so a delivery cannot still be RUNNING when the broker
// redelivers it. That argument is about work that can still be abandoned safely:
// a lock not yet taken, a venue call not yet made. It does not extend to the
// write that records a venue call ALREADY MADE — abandoning that does not undo
// the effect, it only loses the evidence of it, and a cancel the exchange
// executed with no ledger row is strictly worse than a cancel that took two
// seconds too long.
//
// So the commit runs on a context detached from the delivery deadline and
// bounded by its own. Detached also means a shutdown cannot abort it, which is
// the same trade for the same reason: pods stop, exchanges do not forget.
//
// IT IS NOT A WAY TO OPT OUT OF THE BUDGET. It is derived AFTER the venue
// dispatch and used only for the records of it. Everything that decides whether
// to act — the claims, the loads, the venue call itself — keeps the delivery's
// deadline.
func commitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), commitGrace)
}
