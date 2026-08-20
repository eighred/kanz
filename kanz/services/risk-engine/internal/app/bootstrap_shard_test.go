package app

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// BOOT IS WHERE FOREIGN STATE GOT IN (#110).
//
// Bootstrap.restore reads LoadAll — every portfolio in the tenant's database,
// which on a sharded replica is mostly other replicas' books — and used to
// Restore all of it. The copies then froze (ShardFilter drops their live
// events) and Snapshotter.Checkpoint wrote them back over the owners' fresher
// records on the next interval.
//
// The gate now refuses them and the boot path treats that refusal as the
// expected outcome rather than a fault: skipped, counted, and — importantly —
// its resume position is not taken, because this replica is not replaying that
// partition on the owner's behalf.

func recordFor(id v1.PortfolioID, offset uint64) persist.PortfolioRecord {
	asOf := time.Unix(1000, 0).UTC()
	return persist.PortfolioRecord{
		ID:           id,
		BaseCurrency: "USD",
		AsOf:         asOf,
		Positions: []domain.Position{
			{InstrumentID: "AAPL", MarketValue: money(500, "USD"), AsOf: asOf},
		},
		LogPosition: &commonpb.LogPosition{
			Topic:     ingest.EventTypePositionChanged,
			Partition: 0,
			Offset:    offset,
		},
	}
}

// A FOREIGN RECORD IS SKIPPED AND COUNTED; BOOT DOES NOT ABORT.
func TestBootstrapShard_ForeignRecordsAreSkippedNotRestored(t *testing.T) {
	sharding, mine, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	sink := &memSink{recs: []persist.PortfolioRecord{
		recordFor(mine, 10),
		recordFor(theirs, 20),
	}}

	b, err := NewBootstrap(store, sink, sharding.ReplayApplier(store), nil, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run aborted on a foreign record: %v — the gate refusing somebody else's "+
			"portfolio is the boot path working, not failing", err)
	}

	if got := b.ForeignRecordsSkipped(); got != 1 {
		t.Errorf("ForeignRecordsSkipped = %d, want 1", got)
	}
	ids := store.IDs()
	if len(ids) != 1 || ids[0] != mine {
		t.Fatalf("after boot the store holds %v, want exactly [%s] — every extra entry here is one "+
			"the Snapshotter will Save over the owner's record", ids, mine)
	}
}

// A REAL RESTORE FAILURE STILL ABORTS.
//
// The skip is narrow, by error identity: only state.ErrNotOwned. Widening it to
// "log and continue" would boot a replica whose memory is missing a portfolio it
// DOES own and must answer for — and it would answer from nothing.
func TestBootstrapShard_ANonOwnershipFailureStillAborts(t *testing.T) {
	sharding, mine, _ := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	sink := &memSink{recs: []persist.PortfolioRecord{recordFor(mine, 10)}, loadErr: context.DeadlineExceeded}

	b, err := NewBootstrap(store, sink, sharding.ReplayApplier(store), nil, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded with a failing durable read — boot must not proceed on partial state")
	}
}

// AN UNSHARDED REPLICA SKIPS NOTHING. It owns everything, so no record is
// foreign to it, and the count must say so rather than being absent.
func TestBootstrapShard_UnshardedSkipsNothing(t *testing.T) {
	store := state.NewStore()
	sink := &memSink{recs: []persist.PortfolioRecord{recordFor("A", 10), recordFor("B", 20)}}

	b, err := NewBootstrap(store, sink, store, nil, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.ForeignRecordsSkipped(); got != 0 {
		t.Errorf("an unsharded replica skipped %d records, want 0", got)
	}
	if got := len(store.IDs()); got != 2 {
		t.Errorf("the unsharded store restored %d portfolios, want 2 — the deployed posture must "+
			"be untouched by this change", got)
	}
}
