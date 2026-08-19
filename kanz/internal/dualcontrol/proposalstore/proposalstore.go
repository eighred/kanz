// Package proposalstore is the STORAGE half of dual control: where a proposal
// lives between the first signature and the second.
//
// # Why this is a package and not a third private copy
//
// internal/dualcontrol holds the RULE — Propose, Approve, Digest, SameSubject,
// and the state vocabulary a queue renders. It deliberately holds no storage,
// and storage is where the two shipped acts DREW APART:
//
//   - services/datamaster/internal/store/proposals.go (act one, pricing override)
//     grew Lapsed and PurgeLapsed (#563).
//   - services/oms/internal/order/proposals.go (act three, order submission) grew
//     an approver on the row, an expiry announcement (#547), a sweep (#548) and a
//     recorded refusal (#558).
//
// Both then had to restate the same mechanics underneath those differences, and
// they restated them DIFFERENTLY. Measured on main before this package existed,
// datamaster's in-process store accepted three things its own Postgres store
// refuses: a second proposal for an id already held (the map silently overwrote
// the first, so a redelivery could replace the proposer an approver is being
// asked to differ from), a proposal with no proposer (every approver differs
// from "", so the self-approval check passes vacuously), and a nil chosen price
// (which panicked the copy rather than being refused). None of the three was a
// missed line: they were absent because the in-process store had no test at all,
// while the OMS's had a contract run against both of its backends.
//
// "A copied helper is how a fix stops spreading" — #562's act two would have
// been the THIRD copy, and CLAUDE.md's promotion rule fired when the second
// consumer appeared. So the mechanics live here, once, and the contract in
// proposalstoretest runs against every backend of every act.
//
// # What this package owns
//
//   - WELL-FORMEDNESS of the dual-control record. A proposal no store can
//     honestly hold is refused before it is written, in every backend.
//   - The IN-PROCESS backend. One map, one lock, one copy-on-read discipline,
//     one total order. It is the seam most service-level tests run on, so a
//     permissive double here certifies behaviour production does not have —
//     fakeBus already taught this repository what that costs.
//   - The ORDER a queue is read in: oldest first, ties broken by id, so a
//     pending list that reorders between reads cannot read as activity.
//
// # What this package does NOT own, and must not grow
//
// The differences between the acts are real and each one is load-bearing. They
// are expressed by the act, on top of these primitives, rather than flattened:
//
//   - CLAIM DISPOSITION. datamaster DISCARDS the row on claim, because its
//     durable record is the append-only exception_overrides row carrying both
//     names and a second copy here would be a second answer that can disagree
//     with it. The OMS RETAINS the row and writes approver + decided_at onto it,
//     because an admitted order carries the order and not who approved it, so
//     that row is its only evidence two people signed. Remove/Update are the two
//     primitives; neither act is wrong.
//   - EXPIRY DISPOSITION. The OMS announces a terminal ORDER_REJECTED FACT
//     exactly once and sweeps (#547, #548). datamaster announces NOTHING and
//     lists the lapsed proposal until a retention purge takes it (#563), because
//     it publishes one subject nothing but the audit projector consumes — giving
//     it a FACT would be a channel with no reader. What both MUST have, and what
//     the contract enforces, is that a proposal nobody signed is DISCOVERABLE
//     rather than silent: that absence is the whole of #563.
//   - THE SQL. Two tables, two column sets, two sets of CHECK constraints and
//     partial indexes, and in the OMS's case a transaction that commits the
//     proposal together with the FACT announcing it (#292). A shared table with
//     an opaque payload column would erase datamaster's readable reason and
//     chosen_price columns and the OMS's order_proposals_open_idx predicate,
//     which an arch guard enforces — flattening exactly the differences that
//     carry the operational consequence.
//   - THE PAYLOAD. As with dualcontrol.Proposal, the act keeps its own struct and
//     its own digest. This package must not learn what a price or an order is.
package proposalstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// ErrExists is returned when an id is already held.
//
// IT IS A SENTINEL BECAUSE THE CALLER MUST BE ABLE TO ACK ON IT. A redelivered
// command that finds its proposal already held has not failed — the first
// delivery already did the work, and the right answer is to stop quietly rather
// than to nack and redeliver forever. Distinguishing that from a real write
// failure is the difference between a quiet no-op and a poison message.
var ErrExists = errors.New("proposalstore: this id already has a pending proposal")

// Proposal is what an act's stored proposal must be able to answer about itself.
//
// THE ACT KEEPS ITS OWN STRUCT. The payload differs and this package must not
// grow an opinion about it — the same boundary dualcontrol draws for the rule.
// What is shared is the dual-control record inside it and the handful of
// questions a store has to ask.
type Proposal[T any] interface {
	// Record is the dual-control record: id, act, subject, proposer, digest,
	// created and expiry. The store keys, orders, filters and validates on this
	// and nothing else.
	Record() dualcontrol.Proposal

	// WithRecord returns a copy of this proposal carrying r.
	//
	// IT EXISTS FOR THE CONTRACT, and that is a reason rather than an excuse.
	// The shared malformed cases — no proposer, no digest, born expired — are
	// mutations of the SHARED record, which is exactly the part this package
	// owns; without a setter each act would have to hand-write them, which is
	// how a case that matters gets written in one act and forgotten in the next.
	WithRecord(r dualcontrol.Proposal) T

	// Decided reports whether a second signature has been recorded ON THIS ROW.
	//
	// FALSE IS THE CORRECT ANSWER FOR AN ACT THAT DISCARDS ON CLAIM, not a stub:
	// for datamaster the row's continued existence IS its undecidedness, because
	// a decided proposal is not there to be asked. Decided() is what keeps a
	// signed proposal off the pending queue and out of the expiry queue for the
	// act that keeps the row instead.
	Decided() bool

	// Copy returns a DEEP copy. Stored proposals are handed out by value, and a
	// value sharing a pointer with the stored one (a *big.Rat, a protobuf) is not
	// a record of what was proposed — the caller can still mutate it, and then
	// the digest no longer covers what the store holds.
	Copy() T

	// Validate refuses a proposal this act cannot honestly hold. It MUST call
	// Wellformed for the shared record and may add the act's own rules; the
	// contract's malformed cases fail loudly if it does not.
	Validate() error
}

// Wellformed refuses a dual-control record no store of any act can honestly
// hold.
//
// IT RUNS IN EVERY BACKEND, and that is the point rather than a belt-and-braces
// gesture. Each act's Postgres schema refuses most of these again at a CHECK,
// which is the line an auditor's row depends on; a constraint violation is an
// error indistinguishable from a broken migration, so the readable refusal has
// to happen first — and the in-process backend has no CHECK at all, so without
// this it accepts what production refuses.
func Wellformed(p dualcontrol.Proposal) error {
	switch {
	case strings.TrimSpace(p.ID) == "":
		return fmt.Errorf("%w: proposal has no id, so nothing could ever claim it", dualcontrol.ErrMalformed)
	case p.Act == "":
		return fmt.Errorf("%w: proposal names no act, so an approval for any act would cover it", dualcontrol.ErrMalformed)
	case strings.TrimSpace(p.Subject) == "":
		return fmt.Errorf("%w: proposal names no subject, so the approval covers nothing in particular", dualcontrol.ErrMalformed)
	case strings.TrimSpace(p.Proposer) == "":
		// THE ONE THAT FAILS SILENTLY. Every approver differs from an empty
		// proposer, so the self-approval check passes vacuously and one person
		// holds both signatures while the trail shows two.
		return fmt.Errorf("%w: proposal has no proposer, so no approver could ever differ from it", dualcontrol.ErrMalformed)
	case strings.TrimSpace(p.Digest) == "":
		return fmt.Errorf("%w: proposal has no digest, so an approval would cover nothing", dualcontrol.ErrMalformed)
	case p.CreatedAt.IsZero() || p.ExpiresAt.IsZero():
		// UNKNOWN IS NOT FOREVER. A zero expiry read as "never expires" turns a
		// construction bug into an approval that outlives the book it was
		// reasoned about.
		return fmt.Errorf("%w: proposal has no creation time or no expiry — unknown is not forever", dualcontrol.ErrMalformed)
	case !p.ExpiresAt.After(p.CreatedAt):
		return fmt.Errorf("%w: proposal is born expired and could never be approved", dualcontrol.ErrMalformed)
	}
	return nil
}

// Memory is the in-process backend for any act's proposals.
//
// FOR TESTS AND THE SINGLE-REPLICA POSTURE ONLY. With more than one replica a
// proposal held here is approvable only on the pod that took it, so the second
// signature succeeds or fails depending on which pod the approver's request
// reached. A composition root picks it only where it already accepts an
// ephemeral store, and says so.
//
// IT IS DELIBERATELY PRIMITIVE. Insert, Get, Update, Remove, Select and Purge
// are the six operations both shipped acts turned out to need, and the act
// composes its own Claim, Pending, Lapsed or AnnounceExpiry out of them. A
// Claim method here would have to pick one disposition and make the other act
// wrong.
type Memory[T Proposal[T]] struct {
	mu sync.RWMutex
	by map[string]T
}

// NewMemory returns an empty in-process store.
func NewMemory[T Proposal[T]]() *Memory[T] {
	return &Memory[T]{by: map[string]T{}}
}

// Insert stores p if nothing already holds its id, returning ErrExists if
// something does.
//
// commit IS A FALLIBLE SIDE EFFECT THAT MUST NOT SURVIVE A REFUSED INSERT — the
// OMS enqueues the FACT announcing a held order there, because a hold announced
// to nobody is the silent drop the whole control exists to end, and an
// announcement for a hold that did not happen is worse. It runs under the SAME
// lock hold, AFTER the id check and BEFORE the map write: the map write cannot
// fail, so a commit error leaves nothing behind. nil is a store with no
// announcement to make.
//
// THE ID CHECK IS NOT AN OPTIMISATION of a read-then-write. It is the whole
// reason this is one method: a redelivered command must not be able to replace
// the proposer an approver is being asked to differ from.
func (m *Memory[T]) Insert(_ context.Context, p T, commit func() error) error {
	if err := p.Validate(); err != nil {
		return err
	}
	id := p.Record().ID
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.by[id]; ok {
		return ErrExists
	}
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	m.by[id] = p.Copy()
	return nil
}

// Get reads a proposal WITHOUT consuming it, for the checks that must be able to
// refuse without destroying the proposal — a refused self-approval must leave it
// pending for somebody who may actually sign it.
//
// An unknown id is (zero, false, nil) rather than an error: it never existed, it
// was already decided, or another tenant owns it, and the three are deliberately
// indistinguishable.
func (m *Memory[T]) Get(_ context.Context, id string) (T, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.by[id]
	if !ok {
		var zero T
		return zero, false, nil
	}
	return p.Copy(), true, nil
}

// Update is the SERIALISATION POINT for any act that decides a proposal in
// place, and the in-process stand-in for a conditional UPDATE whose rows-affected
// is the verdict.
//
// decide returns the next value, whether to apply it, and an error. All three
// answers are distinct and each has a caller:
//
//   - (_, false, nil) — THIS CALLER DID NOT GET IT. Somebody else already
//     decided it, or the predicate does not hold. Not a failure: an approval that
//     lost the race lost it to a real second signature.
//   - (_, _, err) — refused, and nothing is written. A self-approval lands here,
//     and the proposal is LEFT PENDING.
//   - (next, true, nil) — this caller won; next is stored.
//
// decide RUNS UNDER THE WRITE LOCK, so a fallible side effect that must commit
// with the decision (the OMS enqueues the expiry FACT there) can be done inside
// it: returning an error from decide discards the write, exactly as the Postgres
// rollback discards the transaction.
func (m *Memory[T]) Update(_ context.Context, id string, decide func(T) (T, bool, error)) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.by[id]
	if !ok {
		return false, nil
	}
	next, apply, err := decide(p.Copy())
	if err != nil || !apply {
		return false, err
	}
	m.by[id] = next.Copy()
	return true, nil
}

// Remove deletes a proposal and reports whether THIS caller is the one that got
// it. It is the serialisation point for an act whose durable record lives
// elsewhere, and the reason it is not a bare Delete: two approvers acting on one
// pending proposal at the same moment both pass every check, and both would
// apply. Whoever removes it applies it; the other is told it is already decided.
func (m *Memory[T]) Remove(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.by[id]; !ok {
		return false, nil
	}
	delete(m.by, id)
	return true, nil
}

// Select returns a deep copy of every stored proposal keep accepts, oldest
// first.
func (m *Memory[T]) Select(_ context.Context, keep func(T) bool) ([]T, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]T, 0, len(m.by))
	for _, p := range m.by {
		if keep(p) {
			out = append(out, p.Copy())
		}
	}
	Sort(out)
	return out, nil
}

// Purge removes every proposal drop accepts and reports how many went. It is
// what bounds the table: a proposal nobody signs is never claimed, so without a
// purge every proposal ever left unsigned stays forever.
func (m *Memory[T]) Purge(_ context.Context, drop func(T) bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, p := range m.by {
		if drop(p) {
			delete(m.by, id)
			n++
		}
	}
	return n, nil
}

// Len reports how many proposals are held. For tests that assert a purge or a
// claim actually took the row rather than merely reporting that it did.
func (m *Memory[T]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.by)
}

// Sort puts proposals in the order every queue is read in: oldest first, ties
// broken by id.
//
// THE TIEBREAK IS NOT COSMETIC. Two proposals created in the same nanosecond
// would otherwise list in whichever order the map yielded, and a pending list
// that reorders between two reads reads as activity — somebody deciding whether
// anything happened cannot tell a reshuffle from a new proposal. It is spelled
// once because the two acts had already spelled it twice, with two different
// algorithms.
func Sort[T Proposal[T]](ps []T) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i].Record(), ps[j].Record()
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// Expired reports whether p is past its deadline at now. IT ASKS ABOUT THE
// DEADLINE AND NOTHING ELSE — whether anyone signed it is a separate question,
// and each act adds its own clause for that.
//
// IT IS NOT !someActsPending(). Over the shared record alone it is exactly the
// complement of dualcontrol.Proposal.Pending, so writing it either way here
// would be the same function; the trap is one layer out. Both acts' pending
// queries carry EXTRA clauses — the OMS's excludes a decided proposal and an
// announced one, datamaster's excludes what Claim already removed — and an act
// that spelled its expiry sweep as the negation of its OWN pending predicate
// would treat every signed proposal as expired, announcing or purging acts that
// were carried out. This function exists so the deadline comparison is available
// on its own, spelled once. The two acts had spelled it twice, and only by
// coincidence identically.
//
// EXPIRY IS INCLUSIVE OF THE INSTANT: at ExpiresAt exactly, it is expired. That
// matches dualcontrol.Approve, which refuses an approval at that same instant —
// a second where one said "still signable" and the other said "already lapsed"
// is a second in which an approval is accepted for a proposal the sweeper has
// already announced as dead.
func Expired(p dualcontrol.Proposal, now time.Time) bool {
	return !p.ExpiresAt.After(now.UTC())
}
