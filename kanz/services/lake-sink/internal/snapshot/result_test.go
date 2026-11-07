package snapshot

import (
	"testing"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestWorkerResultClosedProjectionAndPaging(t *testing.T) {
	query := &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: 1}
	zero := uint64(0)
	valid := &lakepb.SnapshotQueryResult{Events: []*lakepb.SnapshotEvent{{Ordinal: 0, EventId: "event", EventType: "market.trade", EventTime: &timestamppb.Timestamp{Seconds: -1, Nanos: 123456789}, DecodeStatus: lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_DECODED}}, NextOrdinal: &zero}
	if err := ValidateResult(query, valid, 2); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*lakepb.SnapshotQueryResult){
		"unknown-time-wire": func(r *lakepb.SnapshotQueryResult) {
			r.Events[0].EventTime.ProtoReflect().SetUnknown([]byte{0x1a, 1, 'x'})
		},
		"unknown-event-wire": func(r *lakepb.SnapshotQueryResult) { r.Events[0].ProtoReflect().SetUnknown([]byte{0x3a, 1, 'x'}) },
		"wrong-arm": func(r *lakepb.SnapshotQueryResult) {
			r.Counts = []*lakepb.SnapshotEventCount{{EventType: "market.trade", Count: 2}}
		},
		"missing-cursor":        func(r *lakepb.SnapshotQueryResult) { r.NextOrdinal = nil },
		"exhausted-with-cursor": func(r *lakepb.SnapshotQueryResult) { r.Exhausted = true },
		"invalid-time":          func(r *lakepb.SnapshotQueryResult) { r.Events[0].EventTime.Nanos = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := proto.Clone(valid).(*lakepb.SnapshotQueryResult)
			mutate(r)
			if ValidateResult(query, r, 2) == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
	query.Operation = lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE
	counts := &lakepb.SnapshotQueryResult{Counts: []*lakepb.SnapshotEventCount{{EventType: "market.trade", Count: 2}}, Exhausted: true}
	if err := ValidateResult(query, counts, 2); err != nil {
		t.Fatal(err)
	}
	counts.Counts[0].Count = 1
	if ValidateResult(query, counts, 2) == nil {
		t.Fatal("partial count accepted as full count")
	}
}
