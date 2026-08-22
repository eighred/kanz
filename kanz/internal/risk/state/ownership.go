package state

import (
	"context"
	"errors"
	"fmt"
	"sync"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// RUNTIME OWNERSHIP: A REPLICA CAN NOW GAIN A PORTFOLIO IT DID NOT BOOT WITH (#110).
//
// # The gap this closes
//
// The Store rebuilds a portfolio by applying every event for it, and it is
// per-replica and in-memory. Until now the only way state entered it was
// Restore, which runs once, single-threaded, during bootstrap. That made a
// portfolio's history a function of WHICH REPLICA WAS ALIVE WHEN, and it is why
// #110's ruling refused both membership mechanisms rather than choosing between
// them:
//
//   - a join-and-watch registry hands a live replica a portfolio mid-life, and
//     the replica has no history for it;
//   - StatefulSet ordinals avoid runtime reassignment only while the fleet does
//     not rescale, and risk-engine is a KEDA-scaled Rollout at 3→12.
//
// Both failure modes are the same one: a replica answering for a portfolio
// whose events it never saw. That is not an outage — the probe is green, the
// measure publishes, and the number is computed from a book that is missing
// positions. Acquire is what makes ownership gain survivable: the replica reads
// the portfolio's durable record and its applied-key tail before it will answer
// for it at all.
//
// # Ownership is OPT-IN, and the default is unchanged
//
// NewStore() with no options is exactly what it was: an ungated store that owns
// every portfolio, lazy-creates on first reference, and is restored at boot.
// That is the deployed single-replica posture (see ShardPosture, which WARNs
// about it by name) and this change does not touch it.
//
// NewStore(WithRuntimeOwnership(loader)) is the gated store a membership
// mechanism will drive. In gated mode a portfolio that has not been Acquired is
// NOT ANSWERABLE and NOT APPLICABLE: every apply refuses with ErrNotOwned and
// SnapshotOwned refuses with ErrNotOwned, instead of lazy-creating an empty
// portfolio whose measures would be a confident zero. Serving zero for a book
// this replica simply does not hold is the silent wrong answer the whole ruling
// is about, so the gate is a refusal, never an empty result.
//
// # What Acquire deliberately does NOT do
//
// It does not decide ownership. There is no ring here, no discovery, no
// heartbeat — #110 defers that choice on purpose, and this file exists so the
// choice is cheap when it is made. The caller (a future membership mechanism)
// says "you own P now"; this says whether the replica can honour that.
//
// # The handoff ordering a mechanism must respect
//
// In-memory state runs AHEAD of the durable record between snapshots, so
// Release returns the final PortfolioRecord it dropped, captured under the same
// per-aggregate lock that serializes applies. A mechanism handing a portfolio
// over must Save that record before the new owner Acquires, or the new owner
// loads a record missing the tail of the old owner's applies. Reading the state
// first and releasing second would lose any apply that landed in between; the
// two are one operation here for exactly that reason.

// Loader is the durable read a runtime Acquire performs. persist.StateStore
// satisfies it — the Store asks for only the method it uses so a mechanism can
// hand it a narrower reader (or a fake) without a second record shape.
type Loader interface {
	Load(ctx context.Context, id v1.PortfolioID) (persist.PortfolioRecord, error)
}

// Option configures a Store at construction. The zero set of options is the
// ungated, owns-everything default.
type Option func(*Store)

// WithRuntimeOwnership puts the Store in gated mode: nothing is answerable
// until it is Acquired, and Acquire reads the portfolio's durable state through
// loader. A nil loader is ignored (the store stays ungated) — a gated store
// with no way to load would refuse every portfolio forever, which is a worse
// failure than the default posture.
func WithRuntimeOwnership(loader Loader) Option {
	return func(s *Store) {
		if loader == nil {
			return
		}
		s.loader = loader
		s.owned = make(map[v1.PortfolioID]struct{})
	}
}

// WithShardOwnership gates the Store on the consistent-hash ring's verdict:
// owns reports whether this replica is the ring's owner of a portfolio.
//
// # The gap this closes — the shard ring did not reach the state it protects
//
// Before this, the ring's ownership was consumed in exactly ONE place: the
// ShardFilter that wraps the LIVE ingest applier. Every other path into and out
// of this Store ignored it, and each of them is a way a portfolio this replica
// does not own gets into its memory and back out to the estate:
//
//   - BOOT. Bootstrap.restore streams LoadEach and Restores every record in the
//     database, unfiltered. A sharded replica therefore starts holding an
//     in-memory copy of EVERY portfolio, most of them somebody else's.
//   - THE COPY THEN FREEZES. ShardFilter drops the live events for those, so
//     the copy never advances past the moment this replica booted.
//   - AND IT IS WRITTEN BACK. Snapshotter.Checkpoint iterates store.IDs() and
//     Saves each one, and persist.Postgres.Save is an unconditional upsert that
//     DELETEs and re-INSERTs the position rows. So every snapshot interval, a
//     non-owner overwrites the owner's fresher durable record with its own
//     boot-frozen one — and the next boot loads the damaged record.
//
// That is not a stale read; it is durable state destruction, and it is what
// "just wire the member lists" would have switched on. Filtering at each of the
// three call sites would leave the fourth to be forgotten, so the gate belongs
// here, with the state: on a ring-gated store a foreign portfolio cannot be
// Restored, cannot be lazy-created by an apply, and therefore never appears in
// IDs() for the snapshotter or the post-bootstrap recompute to find.
//
// A nil owns is ignored (the store stays ungated) — a gate that refuses
// everything is worse than the unsharded default, which at least holds a
// complete book. Composes with WithRuntimeOwnership; see ownsLocked.
func WithShardOwnership(owns func(v1.PortfolioID) bool) Option {
	return func(s *Store) {
		if owns == nil {
			return
		}
		s.shardOwns = owns
	}
}

// Ownership errors.
var (
	// ErrNotOwned: this replica does not own the portfolio, so it has no
	// history for it and must not answer. Callers must surface this as a
	// refusal — never as an empty portfolio, a zero measure, or a fallback to
	// a cached value computed while it DID own it.
	ErrNotOwned = errors.New("state: portfolio is not owned by this replica")
	// ErrOwnershipUnmanaged: Acquire/Release were called on a store built
	// without WithRuntimeOwnership. Silently accepting them would let a
	// mechanism believe it had released a portfolio the store still answers
	// for.
	ErrOwnershipUnmanaged = errors.New("state: store was not built with runtime ownership")
	// ErrEmptyPortfolioID: an ownership call with no aggregate id.
	ErrEmptyPortfolioID = errors.New("state: empty portfolio id")
)

// OwnershipManaged reports whether this Store gates on ownership at all —
// under EITHER source. False is the unsharded default (owns everything).
func (s *Store) OwnershipManaged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owned != nil || s.shardOwns != nil
}

// Owns reports whether this replica may answer for id. Always true on an
// ungated store. Prefer SnapshotOwned for a read: Owns followed by Snapshot is
// two separate acquisitions of mu and a Release can land between them.
func (s *Store) Owns(id v1.PortfolioID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownsLocked(id)
}

// ownsLocked answers the ownership question with mu already held. The gate for
// every apply and every ownership-aware read lives here so there is one
// definition of "may this replica answer".
//
// TWO SOURCES, CONJOINED. A portfolio is owned only if EVERY configured source
// says so: the ring must assign it here (static), and — when a membership
// mechanism is also driving Acquire/Release — it must have been acquired. The
// AND is the safe direction and it is what a dynamic mechanism will want:
// "assigned to me" and "and I have loaded its history" are different
// questions, and answering for a portfolio that satisfies only the first is
// the silent wrong answer this whole gate exists to prevent. With neither
// source configured the store owns everything, which is the unsharded default.
func (s *Store) ownsLocked(id v1.PortfolioID) bool {
	if s.shardOwns != nil && !s.shardOwns(id) {
		return false
	}
	if s.owned == nil {
		return true // no runtime gate: the static verdict above stands
	}
	_, ok := s.owned[id]
	return ok
}

// Acquire takes ownership of id and loads its durable state into memory — the
// runtime counterpart of the boot-time Restore.
//
// IDEMPOTENT: acquiring a portfolio this replica already owns is a no-op that
// returns nil WITHOUT re-reading the durable store. Re-loading would clobber
// live in-memory state with a record that is, by construction, older.
//
// ALL-OR-NOTHING: ownership is granted in the same critical section that
// installs the state, and only after the durable read has succeeded. A load
// failure leaves the store exactly as it was — no ownership, no portfolio, no
// dedup window — so there is no window in which the replica answers from a
// half-loaded book.
//
// NO DURABLE RECORD IS NOT A FAILURE. A portfolio that has never been
// snapshotted has no row (persist.ErrNotFound); the replica takes ownership
// with no in-memory entry, so reads report it unknown (v1.ErrPortfolioNotFound)
// until the first event lazy-creates it — identical to how an unsnapshotted
// portfolio behaves on the ungated store today. What must never happen is
// ownership plus an empty portfolio that reads as a book holding nothing.
func (s *Store) Acquire(ctx context.Context, id v1.PortfolioID) error {
	if id == "" {
		return ErrEmptyPortfolioID
	}
	s.mu.Lock()
	if s.owned == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w (acquire %q)", ErrOwnershipUnmanaged, id)
	}
	// THE RING IS NOT NEGOTIABLE. A membership mechanism driving Acquire must
	// not be able to grant this replica a portfolio the ring assigns elsewhere:
	// two replicas holding the same portfolio is the split the ring exists to
	// prevent, and the second one would overwrite the first's durable record on
	// its next checkpoint.
	if s.shardOwns != nil && !s.shardOwns(id) {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q (the shard ring assigns it to another replica)", ErrNotOwned, id)
	}
	if _, already := s.owned[id]; already {
		s.mu.Unlock()
		return nil
	}
	loader := s.loader
	s.mu.Unlock()

	rec, err := loader.Load(ctx, id)
	haveRecord := true
	switch {
	case errors.Is(err, persist.ErrNotFound):
		haveRecord = false
	case err != nil:
		return fmt.Errorf("state: acquire %q: %w", id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, already := s.owned[id]; already {
		// A concurrent Acquire won. Its load is the one that counts; discarding
		// this one keeps the idempotency guarantee above.
		return nil
	}
	if haveRecord {
		s.portfolios[id] = rec.ToPortfolio()
		if _, ok := s.locks[id]; !ok {
			s.locks[id] = &sync.Mutex{}
		}
		dw := newDedupWindow()
		for _, k := range rec.AppliedKeys {
			dw.Record(k)
		}
		s.dedup[id] = dw
	}
	s.owned[id] = struct{}{}
	return nil
}

// Release gives up ownership of id and drops its in-memory copy. The durable
// record is untouched — Postgres remains the record, and a later Acquire (here
// or on another replica) reads it back.
//
// It returns the state it dropped, captured under the per-aggregate lock so no
// apply can interleave between the read and the drop. That record is what a
// membership mechanism must persist before the next owner acquires: between
// snapshots the in-memory copy is ahead of the durable one, and the events that
// made up the difference were acked long ago.
//
// released is false when the portfolio was not owned — an already-released
// portfolio is not an error, because a watcher re-delivering a revocation is
// normal. err reports only the calls that are structurally wrong: an empty id,
// or a store with no ownership to manage.
func (s *Store) Release(id v1.PortfolioID) (rec persist.PortfolioRecord, released bool, err error) {
	if id == "" {
		return persist.PortfolioRecord{}, false, ErrEmptyPortfolioID
	}
	s.mu.Lock()
	if s.owned == nil {
		s.mu.Unlock()
		return persist.PortfolioRecord{}, false, fmt.Errorf("%w (release %q)", ErrOwnershipUnmanaged, id)
	}
	if _, ok := s.owned[id]; !ok {
		s.mu.Unlock()
		return persist.PortfolioRecord{}, false, nil
	}
	lock := s.locks[id]
	port, hadState := s.portfolios[id]
	dw := s.dedup[id]
	delete(s.owned, id)
	delete(s.portfolios, id)
	delete(s.locks, id)
	delete(s.dedup, id)
	s.mu.Unlock()

	// The lock object outlives its map entry, and an apply that already took it
	// holds the same object — so this serializes against an in-flight apply
	// rather than tearing it. An apply that got its pointers before the delete
	// mutates a portfolio no longer in the map; that write is intentionally
	// lost, which is what "this replica no longer owns it" means.
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	if !hadState {
		return persist.PortfolioRecord{}, true, nil
	}
	var keys []string
	if dw != nil {
		keys = dw.Keys()
	}
	return persist.FromPortfolio(port.Clone(), keys), true, nil
}

// SnapshotOwned is the ownership-aware read: it separates "this replica must
// not answer" from "this replica owns it and has no state yet", which a
// (value, bool) read cannot express.
//
//   - ErrNotOwned — refuse. Do NOT substitute a zero, an empty portfolio, or a
//     cached value computed while this replica did own it. That substitution is
//     the silent wrong answer on the risk path.
//   - v1.ErrPortfolioNotFound — owned, but nothing has been applied or restored
//     for it. The honest "unknown", identical to the ungated store's miss.
//
// The returned portfolio is a deep clone taken under the per-aggregate lock,
// same as Snapshot.
func (s *Store) SnapshotOwned(id v1.PortfolioID) (*domain.Portfolio, error) {
	s.mu.Lock()
	owns := s.ownsLocked(id)
	lock := s.locks[id]
	port, ok := s.portfolios[id]
	s.mu.Unlock()
	if !owns {
		return nil, fmt.Errorf("%w: %q", ErrNotOwned, id)
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", v1.ErrPortfolioNotFound, id)
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	return port.Clone(), nil
}
