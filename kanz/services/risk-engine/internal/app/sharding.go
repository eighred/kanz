package app

import (
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/services/risk-engine/internal/shard"
)

// ONE RING, FOUR CONSUMERS, ONE SOURCE (#110).
//
// A replica's shard ring is not just an ingest filter. Four different parts of
// the process have to agree on "do I own this portfolio", and until now only
// the first of them asked:
//
//	live ingest   — drop another replica's events instead of applying them
//	boot restore  — do not load another replica's durable record into memory
//	log replay    — do not re-apply another replica's history during recovery
//	the query API — do not answer for a portfolio this replica does not hold
//
// Each was wired separately at the composition root, which is the one place in
// this codebase with no unit test around it (cmd/*/main.go). Three of the four
// were simply missing, and the shapes they produced were not outages: a
// non-owner that boots with a frozen copy of every portfolio publishes measures
// from it, serves queries from it, and — via the Snapshotter — writes it back
// over the owner's record. See state.WithShardOwnership for that chain.
//
// Sharding exists so those four cannot come from different places. It holds the
// single validated *shard.Assignment and hands out the wiring each consumer
// needs; main.go never touches the Assignment directly. A test in this package
// can then assert the agreement, which is what the composition root itself
// cannot do.

// Sharding is the replica's shard posture: one validated Assignment, and the
// wiring every ownership-sensitive path derives from it. The zero value is not
// usable — build it with NewSharding.
type Sharding struct {
	assign *shard.Assignment
	self   string
}

// NewSharding wraps the validated Assignment built at startup. A nil assignment
// (or one over an empty ring) is the unsharded default: owns everything, and
// every method below degrades to the pre-sharding behaviour so the composition
// root can wire it unconditionally.
func NewSharding(assign *shard.Assignment, self string) *Sharding {
	return &Sharding{assign: assign, self: self}
}

// Enabled reports whether an effective shard split is in force. False is the
// unsharded single-replica posture ShardPosture WARNs about.
func (s *Sharding) Enabled() bool {
	return s != nil && s.assign != nil && s.assign.Sharded()
}

// Owns reports whether this replica is the ring's owner of id. Always true when
// unsharded.
func (s *Sharding) Owns(id v1.PortfolioID) bool {
	if !s.Enabled() {
		return true
	}
	return s.assign.Owns(string(id))
}

// OwnerOf names the replica the ring assigns id to, or "" when unsharded. It is
// what turns a refusal into something a caller can act on: "not here" is a dead
// end, "replica risk-engine-2 has it" is a route.
func (s *Sharding) OwnerOf(id v1.PortfolioID) string {
	if !s.Enabled() {
		return ""
	}
	return s.assign.Owner(string(id))
}

// StoreOptions gates a state.Store on this ring. Empty when unsharded, so
// NewStore(sharding.StoreOptions()...) is the ungated default store.
//
// This is the backstop the other three consumers rely on: with it in place a
// foreign portfolio cannot enter memory at all, so IDs() — and therefore the
// Snapshotter and the post-bootstrap recompute arming, neither of which asks
// about ownership — only ever sees this replica's own book.
func (s *Sharding) StoreOptions() []state.Option {
	if !s.Enabled() {
		return nil
	}
	return []state.Option{state.WithShardOwnership(s.Owns)}
}

// LiveApplier wraps the live ingest applier so another replica's events are
// dropped BEFORE the recompute trigger, not merely refused by the store. The
// store's gate would reject them anyway, but as an error on every foreign event
// — a correct decision reported as a fault, at spine volume.
func (s *Sharding) LiveApplier(inner ingest.Applier) ingest.Applier {
	if !s.Enabled() {
		return inner
	}
	return NewShardFilter(inner, s.assign)
}

// ReplayApplier is the applier Bootstrap re-applies the durable log through.
// It is the BARE store (replay must not emit a storm of superseded risk FACTs —
// see Bootstrap's doc) with the same ownership filter in front, for the same
// reason LiveApplier has one: recovery must not rebuild another replica's book.
func (s *Sharding) ReplayApplier(store *state.Store) ingest.Applier {
	return s.LiveApplier(store)
}

// EngineOptions makes the query API ownership-aware. Empty when unsharded.
func (s *Sharding) EngineOptions() []engine.EngineOption {
	if !s.Enabled() {
		return nil
	}
	return []engine.EngineOption{engine.WithOwnership(s)}
}

// ConsumerGroup names the durable consumer this replica subscribes under.
//
// A SHARDED REPLICA MUST SEE EVERY EVENT so that the owner among them can apply
// and the rest can drop, which means a per-replica (broadcast) group rather than
// the shared, partition-balanced one. Pairing the ShardFilter with the shared
// group would have every partition delivered to exactly one replica and dropped
// by it whenever it is not the owner — the whole spine discarded.
func (s *Sharding) ConsumerGroup(base string) string {
	if !s.Enabled() {
		return ""
	}
	return base + "-" + s.self
}

// Compile-time proof that Sharding is what the query engine's ownership seam
// asks for, so a change to either signature fails here rather than at the
// untested composition root.
var _ engine.Ownership = (*Sharding)(nil)
