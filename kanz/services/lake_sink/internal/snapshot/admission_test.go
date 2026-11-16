package snapshot

import (
	"errors"
	"testing"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOperationReservationIsNonqueueingAndRetainsUnjoinedLifetime(t *testing.T) {
	w := &Worker{slot: make(chan struct{}, 1)}
	op, err := w.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reserve(); !errors.Is(err, ErrBusy) {
		t.Fatal("busy operation queued/admitted")
	}
	// Exercise lifetime accounting separately from actual process tests: an
	// unjoined child keeps its reference after the public operation closes.
	op.references.Add(1)
	op.Close()
	op.Close()
	if _, err := w.Reserve(); !errors.Is(err, ErrBusy) {
		t.Fatal("unjoined lifetime released admission")
	}
	op.release()
	next, err := w.Reserve()
	if err != nil {
		t.Fatal("joined lifetime did not release admission")
	}
	next.Close()
}

func TestCreateAcceptanceDoesNotRevealProgress(t *testing.T) {
	initial := &lakepb.Snapshot{SnapshotId: "accepted", State: lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED, Selection: &lakepb.SnapshotSelection{EventTypes: []string{"market.trade"}}, CreatedAt: &timestamppb.Timestamp{Seconds: 100}, Custody: lakepb.SnapshotCustody_SNAPSHOT_CUSTODY_LANDED_ROWS_ONLY, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}
	for _, state := range []lakepb.SnapshotState{lakepb.SnapshotState_SNAPSHOT_STATE_CAPTURING, lakepb.SnapshotState_SNAPSHOT_STATE_READY, lakepb.SnapshotState_SNAPSHOT_STATE_FAILED} {
		advanced := proto.Clone(initial).(*lakepb.Snapshot)
		advanced.State = state
		advanced.ManifestSha256 = []byte("private-progress")
		advanced.RowCount = 99
		advanced.ParquetBytes = 123
		advanced.Failure = lakepb.SnapshotFailure_SNAPSHOT_FAILURE_WORKER
		advanced.SealedAt = &timestamppb.Timestamp{Seconds: 200}
		if !proto.Equal(initialAcceptance(advanced), initial) {
			t.Fatal("Create leaked progress without read authorization")
		}
	}
}
