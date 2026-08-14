// Package schedule decides which children of a working parent order should
// exist right now (#435).
//
// # The decision, and why it is pure
//
// internal/execution/algo answers "what is the schedule" as a function of the
// parent's durable fields. This answers the question one layer up: GIVEN that
// schedule, given what the clock says, and given which children already exist,
// which children must this pod create?
//
// It is a pure function of those three things, and deliberately so. Everything
// that makes a parent order survive a restart depends on this decision being
// reproducible: a pod that has been running all day and a pod that booted a
// second ago must reach the same answer, or the second one either re-sends
// children the first already sent or skips them forever.
//
// # There is no cursor, because a child IS the record
//
// The obvious implementation keeps "next slice to send" on the parent. That is
// the #435 trap in its most tempting form — a counter that must be advanced in
// the same breath as a venue message, which is the thing distributed systems
// cannot do. Advance it first and a crash loses a child; advance it second and a
// crash duplicates one.
//
// So nothing is counted. A child's order ID is DERIVED from its parent and its
// slice index (ChildID), which makes the emission idempotent at the store's
// primary key: creating slice 7 twice is the same row, and the second attempt
// loses to a uniqueness violation rather than double-trading. "Which children
// exist" is then a query, not a number somebody maintains — and a query cannot
// drift from the truth it is derived from.
//
// # Clause (c) of #435's Verified-when lives here
//
// "A cancelled parent cancels every unsent child." An UNSENT child is one that
// was never created, so cancelling it means never creating it — and that is
// exactly what a terminal parent yielding no children is. The assertion is
// TestScheduleDue_ATerminalParentEmitsNothing, and it is the reason Parent
// carries Terminal at all rather than the driver checking status somewhere up
// the call stack where a later refactor could lose it.
package schedule

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
)

// ErrNoSchedule is returned for a parent that carries no working schedule. It is
// a programming error rather than a condition: the driver selects parents BY
// their resting status, so one without a schedule means the status and the
// schedule have diverged in the store, and emitting nothing silently would leave
// a parent resting forever with nobody told.
var ErrNoSchedule = errors.New("schedule: parent order carries no execution schedule")

// ErrUnusableParentID refuses a parent whose order ID cannot safely derive child
// IDs — see childSeparator. Admission refuses such an order outright; this is the
// backstop for one that reached the store anyway.
var ErrUnusableParentID = errors.New("schedule: parent order id cannot derive unambiguous child ids")

// childSeparator joins a parent order ID to a slice index.
//
// A COLON, AND THE PARENT ID IS CHECKED FOR IT RATHER THAN ASSUMED FREE OF IT.
//
// The obvious claim to write here is "order ids are UUIDs and a UUID contains no
// colon". IT IS FALSE IN THIS ESTATE, and believing it would be a real defect:
// the gateway only DEFAULTS order_id to a UUID when the client leaves it empty
// (services/api-gateway/internal/orders/orders.go), so a client may supply any
// string it likes. A client-supplied parent "a:1" would derive the same child id
// for its slice 0 as parent "a" derives for its slice 1 — and because that id is
// the store's primary key AND the venue's clientOrderId, the collision does not
// produce an error. It merges two orders.
//
// So ErrUnusableParentID refuses a parent whose id contains this separator,
// rather than a comment asserting the case away.
const childSeparator = ":"

// Parent is a working parent order, reduced to what the decision needs.
//
// A STRUCT RATHER THAN THE PROTO, so this decision cannot quietly grow a
// dependency on order state that is not part of it. Everything here is either
// durable on the order or derived from fields that are.
type Parent struct {
	// OrderID is the parent's own ID; children derive theirs from it.
	OrderID string

	// Terminal is whether the parent has reached a terminal status —
	// cancelled, rejected, expired, or filled.
	//
	// THIS IS CLAUSE (c). A terminal parent yields no children, which is what
	// "a cancelled parent cancels every unsent child" means when an unsent
	// child is one that was never created.
	Terminal bool

	// Plan is the schedule, derived from the parent's durable fields.
	Plan algo.Plan
}

// Child is one child order the driver should create.
type Child struct {
	// OrderID is derived, not allocated. See ChildID.
	OrderID string
	// ParentID is stamped on the child so the relation is durable in both
	// directions — the driver reads children BY parent, and an operator holding
	// a child needs to reach its parent without parsing an ID.
	ParentID string
	Index    int
	Quantity *big.Rat
	// Due is when this child became due, NOT when it is being created. The two
	// differ by up to one driver tick, and recording the scheduled time is what
	// lets a later reader see that a child went out late without having to
	// reconstruct the tick that sent it.
	Due time.Time
}

// ChildID derives a child order's ID from its parent and slice index.
//
// DERIVED, NEVER ALLOCATED, and that is the whole idempotency mechanism. Two
// pods that both decide slice 7 is due compute the same ID, so the store's
// primary key rejects the second — the duplicate is refused by the database
// rather than avoided by a lock this platform would have to hold across a venue
// call. A random child ID would make every retry a new order, and a retry that
// double-trades is the failure mode #292 and the PENDING_NEW sweep exist to end.
func ChildID(parentID string, index int) string {
	return parentID + childSeparator + strconv.Itoa(index)
}

// UsableParentID reports whether an order ID can safely be worked as a schedule.
//
// CALLED AT ADMISSION, BEFORE THE ORDER IS ACCEPTED, so a client that cannot be
// sliced is told at the only moment it can still do something about it. Called
// again in Due as a backstop, because the cost of being wrong is not an error —
// it is two orders merged at a primary key.
//
// The two refusals:
//
//   - AN EMPTY ID would derive children ":0", ":1" … which EVERY unnamed parent
//     would share.
//   - AN ID CONTAINING THE SEPARATOR makes the derivation ambiguous: parent "a:1"
//     slice 0 and parent "a" slice 1 both spell "a:1:0"… and, one level up, "a:1"
//     is itself what a child of "a" is called. That second case is also what
//     stops a CHILD being scheduled as a parent, which would otherwise nest
//     without limit.
func UsableParentID(orderID string) error {
	if orderID == "" {
		return fmt.Errorf("%w: the order has no id, and every unnamed parent would derive the "+
			"same child ids", ErrUnusableParentID)
	}
	if strings.Contains(orderID, childSeparator) {
		return fmt.Errorf("%w: order id %q contains %q, which is how a child id is spelled — "+
			"its children could not be told apart from another parent's, and the store would "+
			"merge them rather than refuse them", ErrUnusableParentID, orderID, childSeparator)
	}
	return nil
}

// ParseChildID reports the parent and slice index encoded in a child order ID,
// and whether the ID is one this package derived.
//
// The driver does not need this — it queries children by parent_order_id, which
// is a column. It exists for the operator path: given a child order ID from a
// venue report or an alert, say which slice of which parent it is.
func ParseChildID(childID string) (parentID string, index int, ok bool) {
	cut := strings.LastIndex(childID, childSeparator)
	if cut <= 0 || cut == len(childID)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(childID[cut+1:])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return childID[:cut], n, true
}

// Due returns the children that are due at now and do not already exist.
//
// exists reports whether a child order ID is already in the store. It is a
// PREDICATE RATHER THAN A COUNT for the reason the package doc gives: a count
// assumes children were created in order and none was lost, and neither is
// guaranteed — a crash between two creations leaves a hole, and a count would
// skip past it forever while the parent never completes.
//
// The result is ordered by slice index, so a driver that stops early on the
// first failure has sent a PREFIX of the schedule rather than an arbitrary
// subset. That matters on the next tick: a hole is refilled before anything
// later is added, which is what keeps a partially-sent parent recoverable.
func Due(p Parent, exists func(childID string) bool, now time.Time) ([]Child, error) {
	// CLAUSE (c) OF #435, AND IT IS FIRST FOR A REASON.
	//
	// Not "return early as an optimisation" — this is the cancellation. An
	// unsent child is one that was never created, so a terminal parent that
	// yields nothing IS a cancelled parent cancelling every unsent child.
	// Checked before the schedule is even computed, so no ordering of the
	// clauses below can leak a child past a cancel.
	if p.Terminal {
		return nil, nil
	}
	if p.Plan.Total == nil || p.Plan.Slices <= 0 {
		return nil, fmt.Errorf("%w: order %s", ErrNoSchedule, p.OrderID)
	}
	if err := UsableParentID(p.OrderID); err != nil {
		return nil, err
	}

	slices, err := algo.TWAP(p.Plan)
	if err != nil {
		return nil, fmt.Errorf("schedule: parent %s carries an unworkable schedule: %w", p.OrderID, err)
	}

	var out []Child
	for _, sl := range algo.Due(slices, now) {
		id := ChildID(p.OrderID, sl.Index)
		if exists(id) {
			continue
		}
		out = append(out, Child{
			OrderID:  id,
			ParentID: p.OrderID,
			Index:    sl.Index,
			Quantity: sl.Quantity,
			Due:      sl.Due,
		})
	}
	return out, nil
}

// Complete reports whether every child of this parent has been created — i.e.
// the schedule has been worked to its end.
//
// IT IS NOT "THE PARENT IS DONE". Every child existing means every child was
// SENT; they may still be working at a venue, and a parent goes terminal on what
// its children DID, not on what was sent to them. Conflating the two would
// retire a parent while its last slice was still live, and the fills that
// arrived after would fold onto an order the platform had stopped watching.
func Complete(p Parent, exists func(childID string) bool) bool {
	if p.Plan.Slices <= 0 {
		return false
	}
	for i := range p.Plan.Slices {
		if !exists(ChildID(p.OrderID, i)) {
			return false
		}
	}
	return true
}
