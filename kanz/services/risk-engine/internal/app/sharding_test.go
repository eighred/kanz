package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/services/risk-engine/internal/shard"
)

// ONE RING, FOUR CONSUMERS (#110).
//
// These are the assertions the composition root cannot make about itself.
// cmd/risk-engine/main.go is package main and untested, and it is exactly where
// the ring's ownership used to reach only ONE of the four paths that need it —
// the live ingest filter — while boot restore, log replay and the query API all
// ignored it. Sharding is the single value main.go now derives all four from,
// so a disagreement is a compile error or a failure here rather than a fleet
// overwriting its own durable records.

// ringFixture builds a real three-member Assignment and finds one portfolio id
// the ring assigns to self and one it assigns elsewhere. Ownership is a pure
// function of (members, key), so this is deterministic, not a probe.
func ringFixture(t *testing.T) (sharding *Sharding, mine, theirs v1.PortfolioID) {
	t.Helper()
	members := []string{"risk-engine-0", "risk-engine-1", "risk-engine-2"}
	const self = "risk-engine-1"
	assign, err := shard.NewAssignment(shard.NewRing(members, 0), self)
	if err != nil {
		t.Fatalf("NewAssignment: %v", err)
	}
	sharding = NewSharding(assign, self)

	for i := 0; i < 200 && (mine == "" || theirs == ""); i++ {
		id := v1.PortfolioID(fmt.Sprintf("PORT-%03d", i))
		if sharding.Owns(id) {
			if mine == "" {
				mine = id
			}
			continue
		}
		if theirs == "" {
			theirs = id
		}
	}
	if mine == "" || theirs == "" {
		t.Fatalf("a 3-member ring split 200 keys into one bucket (mine=%q theirs=%q) — the ring "+
			"is not distributing and every test below would be vacuous", mine, theirs)
	}
	return sharding, mine, theirs
}

func posEnvelope(id v1.PortfolioID) (*envelopepb.Envelope, *domainpb.PositionState) {
	return &envelopepb.Envelope{IdempotencyKey: string(id) + "-k", PartitionKey: string(id)},
		&domainpb.PositionState{
			PortfolioId:  string(id),
			InstrumentId: "AAPL",
			AsOf:         timestamppb.New(time.Unix(1000, 0).UTC()),
		}
}

// THE STORE IS GATED BY THE SAME RING THE FILTER USES.
//
// This is the backstop the other three consumers lean on: the Snapshotter and
// ArmPostBootstrapRecomputes iterate store.IDs() without asking about ownership,
// which is only safe because a foreign portfolio cannot get into the store.
func TestSharding_StoreOptionsGateOnTheSameRing(t *testing.T) {
	sharding, mine, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)

	if !store.Owns(mine) {
		t.Errorf("the store disowns %q, which the ring assigns to this replica", mine)
	}
	if store.Owns(theirs) {
		t.Errorf("the store owns %q, which the ring assigns to %q — the store gate and the ring "+
			"disagree", theirs, sharding.OwnerOf(theirs))
	}
}

// REPLAY IS FILTERED TOO. Recovery must not rebuild another replica's book: the
// durable log carries every portfolio, and replay used to apply all of it
// through the bare store.
func TestSharding_ReplayApplierDropsForeignEvents(t *testing.T) {
	sharding, mine, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	applier := sharding.ReplayApplier(store)

	env, pos := posEnvelope(theirs)
	if err := applier.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("replay of a foreign event returned %v; the filter must DROP it, not fault — "+
			"at log volume a correct decision reported as an error is an unreadable boot", err)
	}
	if _, found := store.Snapshot(theirs); found {
		t.Errorf("replay rebuilt %q, which belongs to %q", theirs, sharding.OwnerOf(theirs))
	}

	env, pos = posEnvelope(mine)
	if err := applier.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("replay of an owned event: %v", err)
	}
	if _, found := store.Snapshot(mine); !found {
		t.Errorf("replay dropped %q, which this replica owns — recovery lost its own book", mine)
	}
}

// THE LIVE FILTER AGREES WITH THE STORE GATE.
func TestSharding_LiveApplierDropsForeignEvents(t *testing.T) {
	sharding, mine, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	applier := sharding.LiveApplier(store)

	env, pos := posEnvelope(theirs)
	if err := applier.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("live apply of a foreign event = %v, want a silent drop", err)
	}
	env, pos = posEnvelope(mine)
	if err := applier.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("live apply of an owned event: %v", err)
	}
	ids := store.IDs()
	if len(ids) != 1 || ids[0] != mine {
		t.Errorf("store holds %v, want exactly [%s]", ids, mine)
	}
}

// A REFUSAL NAMES THE OWNER. "Not here" is a dead end; the replica's id is a
// route, and it is what separates this from a bare not-found.
func TestSharding_OwnerOfNamesARealMember(t *testing.T) {
	sharding, _, theirs := ringFixture(t)
	owner := sharding.OwnerOf(theirs)
	if owner == "" {
		t.Fatal("OwnerOf returned empty for a portfolio the ring assigns elsewhere")
	}
	if owner == "risk-engine-1" {
		t.Fatalf("OwnerOf(%q) = self, but Owns said otherwise — the two disagree", theirs)
	}
}

// A SHARDED REPLICA MUST SEE EVERY EVENT, so it subscribes under its OWN durable
// consumer rather than the shared partition-balanced one. Pairing the filter
// with the shared group would have each partition delivered to exactly one
// replica and dropped by it whenever it is not the owner — the whole spine
// discarded, silently.
func TestSharding_ShardedReplicaGetsItsOwnConsumerGroup(t *testing.T) {
	sharding, _, _ := ringFixture(t)
	group := sharding.ConsumerGroup(DefaultConsumerGroup)
	if group == DefaultConsumerGroup || group == "" {
		t.Fatalf("ConsumerGroup = %q — a sharded replica on the shared group drops the spine", group)
	}
	if want := DefaultConsumerGroup + "-risk-engine-1"; group != want {
		t.Errorf("ConsumerGroup = %q, want %q", group, want)
	}
}

// THE UNSHARDED DEFAULT IS TRANSPARENT IN EVERY DIRECTION.
//
// This is the deployed posture. If wiring Sharding unconditionally changed it,
// the change would ship a behaviour switch to every single-replica deployment
// rather than to the fleets that opt in.
func TestSharding_UnshardedIsTransparent(t *testing.T) {
	assign, err := shard.NewAssignment(shard.NewRing(nil, 0), "")
	if err != nil {
		t.Fatalf("NewAssignment for the unsharded default: %v", err)
	}
	s := NewSharding(assign, "")

	if s.Enabled() {
		t.Error("an empty ring reports Enabled")
	}
	if !s.Owns("ANYTHING") {
		t.Error("an unsharded replica disowned a portfolio — it owns everything by definition")
	}
	if opts := s.StoreOptions(); len(opts) != 0 {
		t.Errorf("StoreOptions = %d options unsharded, want 0 (an ungated store)", len(opts))
	}
	if opts := s.EngineOptions(); len(opts) != 0 {
		t.Errorf("EngineOptions = %d options unsharded, want 0", len(opts))
	}
	if g := s.ConsumerGroup(DefaultConsumerGroup); g != "" {
		t.Errorf("ConsumerGroup = %q unsharded, want \"\" (the shared partition-balanced default)", g)
	}

	store := state.NewStore(s.StoreOptions()...)
	if store.OwnershipManaged() {
		t.Error("an unsharded replica built a gated store")
	}
	if _, wrapped := s.LiveApplier(store).(*ShardFilter); wrapped {
		t.Error("LiveApplier wrapped the applier on an unsharded replica")
	}
}
