package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// THE SHARD RING MUST REACH THE STATE IT PROTECTS (#110).
//
// The corruption these tests pin down needs no cluster to reason about and is
// entirely a property of one replica:
//
//	Bootstrap.restore streams the whole database via LoadEach and Restores each record.
//	ShardFilter then drops the live events for the ones this replica does not
//	own, so those copies freeze at boot. Snapshotter.Checkpoint iterates
//	store.IDs() and Saves every entry, and persist.Postgres.Save is an
//	unconditional upsert that DELETEs and re-INSERTs the position rows.
//
// So a non-owner overwrote the OWNER's fresher durable record with its
// boot-frozen one, every snapshot interval, and the next boot read back the
// damaged record. Nothing errored.
//
// The fix is that ownership lives with the state rather than at the three call
// sites that would each have to remember it: on a ring-gated store a foreign
// portfolio cannot be Restored and cannot be lazy-created, so it never reaches
// IDs() at all — and IDs() is exactly what the Snapshotter and the
// post-bootstrap recompute arming iterate without asking about ownership.

// ringOf gates on an explicit owned set — the deterministic stand-in for a
// consistent-hash verdict, which is a pure function of (members, key) and needs
// no ring here to be exercised.
func ringOf(owned ...v1.PortfolioID) func(v1.PortfolioID) bool {
	set := make(map[v1.PortfolioID]struct{}, len(owned))
	for _, id := range owned {
		set[id] = struct{}{}
	}
	return func(id v1.PortfolioID) bool {
		_, ok := set[id]
		return ok
	}
}

func shardedStore(owned ...v1.PortfolioID) *state.Store {
	return state.NewStore(state.WithShardOwnership(ringOf(owned...)))
}

func portfolioAt(id v1.PortfolioID, asOf time.Time) *domain.Portfolio {
	p := domain.NewPortfolio(id, "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: "USD"})
	return p
}

func positionEvent(id v1.PortfolioID, mv int64, asOf time.Time) (*envelopepb.Envelope, *domainpb.PositionState) {
	return &envelopepb.Envelope{IdempotencyKey: string(id) + "-k", PartitionKey: string(id)},
		&domainpb.PositionState{
			PortfolioId:  string(id),
			InstrumentId: "AAPL",
			MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: mv}, CurrencyCode: "USD"},
			AsOf:         timestamppb.New(asOf),
		}
}

// A FOREIGN DURABLE RECORD CANNOT ENTER MEMORY.
//
// This is the head of the corruption chain. Restore is the boot path's only
// door, and on a ring-gated store it refuses an id the ring assigns elsewhere.
func TestShardOwnership_RestoreRefusesAForeignRecord(t *testing.T) {
	s := shardedStore("MINE")
	asOf := time.Unix(1000, 0).UTC()

	if err := s.Restore(portfolioAt("MINE", asOf), []string{"k1"}); err != nil {
		t.Fatalf("Restore of an owned portfolio: %v", err)
	}
	err := s.Restore(portfolioAt("THEIRS", asOf), []string{"k2"})
	if !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("Restore of a foreign record = %v, want ErrNotOwned — this is the read that used "+
			"to load every other replica's book into this one", err)
	}
}

// AND THEREFORE THE SNAPSHOTTER CANNOT WRITE IT BACK.
//
// The Snapshotter and ArmPostBootstrapRecomputes both iterate store.IDs() and
// neither asks about ownership; that is safe only because a foreign portfolio
// never gets into the maps IDs() reads. This asserts the property they depend
// on rather than their code, so it holds for any future IDs() consumer too.
func TestShardOwnership_ForeignPortfoliosNeverReachIDs(t *testing.T) {
	s := shardedStore("MINE")
	asOf := time.Unix(1000, 0).UTC()

	_ = s.Restore(portfolioAt("MINE", asOf), nil)
	_ = s.Restore(portfolioAt("THEIRS", asOf), nil)
	// A lazy-create is the other way in: an apply for an unknown portfolio
	// creates it. The gate is inside the same critical section as that create.
	env, pos := positionEvent("THEIRS", 500, asOf)
	if err := s.ApplyPositionChanged(context.Background(), env, pos); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("apply for a foreign portfolio = %v, want ErrNotOwned", err)
	}

	ids := s.IDs()
	if len(ids) != 1 || ids[0] != "MINE" {
		t.Fatalf("IDs() = %v, want exactly [MINE] — anything else here is a portfolio the "+
			"Snapshotter will Save over the owner's durable record", ids)
	}
	if _, found := s.Snapshot("THEIRS"); found {
		t.Error("Snapshot returned a foreign portfolio — the query path computes from this")
	}
}

// THE OWNED HALF IS COMPLETELY UNAFFECTED.
//
// A gate that quietly cost the replica its own portfolios would be worse than
// the bug: it is the "owns nothing, green probes" failure #568 already refused
// at startup, arriving through a different door.
func TestShardOwnership_OwnedPortfoliosBehaveExactlyAsBefore(t *testing.T) {
	s := shardedStore("MINE")
	asOf := time.Unix(1000, 0).UTC()

	env, pos := positionEvent("MINE", 500, asOf)
	if err := s.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("apply for an owned portfolio: %v", err)
	}
	p, found := s.Snapshot("MINE")
	if !found {
		t.Fatal("an owned portfolio was lazy-created and then not found")
	}
	if got := len(p.Positions()); got != 1 {
		t.Errorf("the owned portfolio holds %d positions, want 1 — the applied event did not land", got)
	}
}

// AN UNGATED STORE IS UNCHANGED — the deployed unsharded posture.
func TestShardOwnership_NoRingOwnsEverything(t *testing.T) {
	s := state.NewStore()
	if s.OwnershipManaged() {
		t.Error("NewStore() with no options reports ownership management")
	}
	if err := s.Restore(portfolioAt("ANY", time.Unix(1000, 0).UTC()), nil); err != nil {
		t.Fatalf("ungated Restore: %v", err)
	}
	if !s.Owns("ANYTHING-AT-ALL") {
		t.Error("an ungated store disowned a portfolio")
	}
}

// --- composition with the runtime (Acquire/Release) source ---------------

type recordLoader struct {
	rec  persist.PortfolioRecord
	have bool
	hits int
}

func (l *recordLoader) Load(context.Context, v1.PortfolioID) (persist.PortfolioRecord, error) {
	l.hits++
	if !l.have {
		return persist.PortfolioRecord{}, persist.ErrNotFound
	}
	return l.rec, nil
}

// THE RING IS NOT NEGOTIABLE BY A MEMBERSHIP MECHANISM.
//
// Acquire is the door a future discovery mechanism drives. If it could grant a
// portfolio the ring assigns elsewhere, two replicas would hold the same book
// and the second would overwrite the first's durable record on its next
// checkpoint — the exact split the ring exists to prevent, reintroduced by the
// thing meant to operate it.
func TestShardOwnership_AcquireCannotOverrideTheRing(t *testing.T) {
	loader := &recordLoader{}
	s := state.NewStore(
		state.WithShardOwnership(ringOf("MINE")),
		state.WithRuntimeOwnership(loader),
	)

	err := s.Acquire(context.Background(), "THEIRS")
	if !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("Acquire of a portfolio outside the ring = %v, want ErrNotOwned", err)
	}
	if loader.hits != 0 {
		t.Errorf("the durable store was read %d times for a portfolio the ring refuses; the "+
			"refusal must precede the load", loader.hits)
	}
	if err := s.Acquire(context.Background(), "MINE"); err != nil {
		t.Fatalf("Acquire of a ring-owned portfolio: %v", err)
	}
}

// THE TWO SOURCES ARE CONJOINED, NOT ALTERNATIVES.
//
// Ring-assigned but not yet acquired must still refuse: "the ring says this is
// mine" and "and I have loaded its history" are different questions, and
// answering on the first alone is a book with no positions in it reported as a
// book holding nothing.
func TestShardOwnership_RingAssignedButUnacquiredStillRefuses(t *testing.T) {
	s := state.NewStore(
		state.WithShardOwnership(ringOf("MINE")),
		state.WithRuntimeOwnership(&recordLoader{}),
	)
	if s.Owns("MINE") {
		t.Fatal("Owns(MINE) before Acquire — the ring's verdict alone made it answerable")
	}
	if _, err := s.SnapshotOwned("MINE"); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("SnapshotOwned before Acquire = %v, want ErrNotOwned", err)
	}
	if err := s.Acquire(context.Background(), "MINE"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !s.Owns("MINE") {
		t.Error("Owns(MINE) is false after a successful Acquire")
	}
}
