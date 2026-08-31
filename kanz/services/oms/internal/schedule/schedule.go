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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/orderid"
)

// ErrNoSchedule is returned for a parent that carries no working schedule. It is
// a programming error rather than a condition: the driver selects parents BY
// their resting status, so one without a schedule means the status and the
// schedule have diverged in the store, and emitting nothing silently would leave
// a parent resting forever with nobody told.
var ErrNoSchedule = errors.New("schedule: parent order carries no execution schedule")

// ErrUnusableParentID refuses a parent whose order ID cannot safely derive child
// IDs — see internal/orderid. Admission refuses such an order outright; this is
// the
// backstop for one that reached the store anyway.
var ErrUnusableParentID = errors.New("schedule: parent order id cannot derive unambiguous child ids")

// childSalt separates a parent order ID from the slice index before they are
// hashed. It never appears in a child ID, so it is free of the constraint the old
// separator carried: it no longer has to be a character a parent ID cannot
// contain, because the two values reach the hash distinctly.
const childSalt = ":"

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

	// Plan is the schedule, derived from the parent's durable fields — including
	// which algorithm works it (algo.Plan.Algo, #868). An unset or unimplemented
	// name is refused by Due rather than defaulted.
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
// # Derived, never allocated — that is the idempotency mechanism
//
// Two pods that both decide slice 7 is due compute the same ID, so the store's
// primary key refuses the second: the duplicate is rejected by the database
// rather than avoided by a lock this platform would have to hold across a venue
// call. A random child ID would make every retry a new order, and a retry that
// double-trades is the failure #292 and the PENDING_NEW sweep exist to prevent.
//
// # Why it is a HASH and not "<parent>:<index>"
//
// That was the readable form, and it could never have been placed. A child is an
// ORDER, so its ID is stamped as the venue's client order ID — and OKX accepts
// at most 32 characters, letters and digits only (internal/orderid records the
// probe that established this). "<parent>:<index>" breaks both rules at once:
// the colon is refused outright, and a 32-character parent leaves no room for
// any suffix. Every scheduled child order would have been admitted, stored,
// announced, and then refused by the exchange with an opaque parameter error.
//
// Hashing gives a value that is deterministic, exactly 32 hex characters, and
// collision-resistant across BOTH inputs — so the separator no longer has to be
// a character a parent ID cannot contain: the parent and the index reach the
// hash as distinct inputs rather than as one concatenated string.
//
// THE COST IS LEGIBILITY, and it is a real one: "p1:0" told an operator at a
// glance what they were looking at. The relation survives in the parent_order_id
// column — which is what the driver queries anyway — and the slice index is
// logged when a child is sent. A readable ID that cannot be placed is worth less
// than an opaque one that can.
func ChildID(parentID string, index int) string {
	sum := sha256.Sum256([]byte(parentID + childSalt + strconv.Itoa(index)))
	return hex.EncodeToString(sum[:orderid.MaxLen/2])
}

// UsableParentID reports whether an order can be worked as a schedule.
//
// CALLED AT ADMISSION, BEFORE THE ORDER IS ACCEPTED, so an order that cannot be
// sliced is refused at the only moment somebody can still do something about it.
// Called again in Due as a backstop.
//
// The rule is internal/orderid's, not a local one. A parent never reaches a
// venue itself — that is what WORKING_SCHEDULED means — so strictly it could
// carry any ID. Holding it to the same rule as every other order is deliberate:
// an order ID no venue could accept is a defect wherever it appears, and
// exempting parents would leave the gap open in the one place nothing else looks.
func UsableParentID(orderID string) error {
	if err := orderid.Valid(orderID); err != nil {
		return fmt.Errorf("%w: %s", ErrUnusableParentID, err)
	}
	return nil
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

	// THE ALGORITHM COMES OFF THE PARENT, NOT OUT OF THIS FILE (#868).
	//
	// This used to name TWAP. That made "how is this order worked" a property of
	// the build rather than of the order, so a desk could not choose per order and
	// a second algorithm could not land without editing this decision. algo.Run
	// resolves p.Plan.Algo through the registry and REFUSES a name this build does
	// not implement — never a fallback, because a parent worked by an algorithm
	// nobody asked for would still produce fills, and every reader of them would
	// see the label of an algorithm that never ran.
	//
	// What is passed alongside is deliberate on both counts. Sent is a predicate
	// over slice indices rather than a count, for the reason this package's doc
	// gives — a count skips past a hole forever. UnknownMarket is passed
	// EXPLICITLY: this decision has no market data, and saying so is not the same
	// as saying nothing. An algorithm that needs a book or a volume profile
	// (#867) refuses here rather than inventing one.
	state := algo.ParentState{
		OrderID: p.OrderID,
		Sent:    func(index int) bool { return exists(ChildID(p.OrderID, index)) },
	}
	slices, err := algo.Run(p.Plan, state, algo.UnknownMarket{})
	if errors.Is(err, algo.ErrUnknownAlgo) {
		return nil, fmt.Errorf("schedule: parent %s names an execution algorithm this build cannot "+
			"work, so nothing can advance it: %w", p.OrderID, err)
	}
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
