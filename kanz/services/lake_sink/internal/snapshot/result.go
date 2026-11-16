package snapshot

import lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"

// ValidateResult checks the closed operation-specific response before any data
// leaves the service. A valid protobuf alone is not a valid query result.
func ValidateResult(query *lakepb.SnapshotQuery, result *lakepb.SnapshotQueryResult, rowCount uint64) error {
	if ValidateQuery(query) != nil || result == nil || rowCount > MaxRows || len(result.ProtoReflect().GetUnknown()) != 0 {
		return ErrWorker
	}
	if query.Operation == lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE {
		if len(result.Events) != 0 || result.NextOrdinal != nil || !result.Exhausted || len(result.Counts) > int(query.PageSize) {
			return ErrWorker
		}
		seen := make(map[string]bool, len(result.Counts))
		var total uint64
		for _, count := range result.Counts {
			if count == nil || !boundedText(count.EventType, 256) || count.Count == 0 || count.Count > rowCount-total || seen[count.EventType] || len(count.ProtoReflect().GetUnknown()) != 0 {
				return ErrWorker
			}
			seen[count.EventType] = true
			total += count.Count
		}
		if total != rowCount {
			return ErrWorker
		}
		return nil
	}
	if len(result.Counts) != 0 || len(result.Events) > int(query.PageSize) || uint64(len(result.Events)) > rowCount {
		return ErrWorker
	}
	var previous uint64
	for i, event := range result.Events {
		if event == nil || !boundedText(event.EventId, 256) || !boundedText(event.EventType, 256) || event.EventTime == nil || len(event.EventTime.ProtoReflect().GetUnknown()) != 0 || event.EventTime.CheckValid() != nil || event.DecodeStatus < 1 || event.DecodeStatus > 3 || len(event.ProtoReflect().GetUnknown()) != 0 {
			return ErrWorker
		}
		if event.Ordinal >= rowCount || (i > 0 && event.Ordinal <= previous) || (query.AfterOrdinal != nil && event.Ordinal <= *query.AfterOrdinal) {
			return ErrWorker
		}
		if query.Operation == lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_DECODE_FAILURES && event.DecodeStatus != lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_FAILED {
			return ErrWorker
		}
		previous = event.Ordinal
	}
	if result.Exhausted {
		if result.NextOrdinal != nil {
			return ErrWorker
		}
	} else if len(result.Events) != int(query.PageSize) || result.NextOrdinal == nil || *result.NextOrdinal != previous {
		return ErrWorker
	}
	return nil
}
