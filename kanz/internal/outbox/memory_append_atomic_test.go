package outbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestMemoryAppendRefusesEntireInvalidBatchBeforeMutation(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		for _, invalid := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("seeded=%t/invalid=%d", seeded, invalid), func(t *testing.T) {
				ctx := context.Background()
				q := NewMemory()
				clockCalls := 0
				q.nowFunc = func() time.Time {
					clockCalls++
					return time.Unix(1_700_000_000, 0).Add(time.Duration(clockCalls) * time.Second)
				}
				var previous []Record
				if seeded {
					previous = []Record{fact(t, testCtx(), "order.order.cancelled", "existing")}
					if err := q.Append(previous...); err != nil {
						t.Fatal(err)
					}
					if err := q.MarkFailed(ctx, q.rows[0].id, errors.New("existing failure")); err != nil {
						t.Fatal(err)
					}
				}
				before := make([]memRow, len(q.rows))
				for i, row := range q.rows {
					before[i] = *row
				}
				next, calls := q.next, clockCalls
				valid := []Record{
					fact(t, testCtx(), "order.order.cancelled", "first"),
					fact(t, testCtx(), "order.order.cancelled", "middle"),
					fact(t, testCtx(), "order.order.cancelled", "last"),
				}
				mixed := append([]Record(nil), valid...)
				mixed[invalid].TenantID = ""
				if err := q.Append(mixed...); err == nil || err.Error() != "outbox: record with no tenant" {
					t.Fatalf("mixed batch error = %v, want the tenant refusal", err)
				}
				if q.next != next || clockCalls != calls || len(q.rows) != len(before) {
					t.Fatalf("refused batch mutated counter/clock/depth: next=%d/%d calls=%d/%d rows=%d/%d", q.next, next, clockCalls, calls, len(q.rows), len(before))
				}
				for i, row := range q.rows {
					if !reflect.DeepEqual(*row, before[i]) {
						t.Fatalf("preexisting row %d changed after refusal", i)
					}
				}
				if err := q.Append(valid...); err != nil {
					t.Fatalf("valid retry: %v", err)
				}
				want := append(previous, valid...)
				if q.PendingCount() != len(want) || clockCalls != calls+len(valid) {
					t.Fatalf("retry retained phantom records or read the clock incorrectly: depth=%d calls=%d", q.PendingCount(), clockCalls)
				}
				var keys []string
				lastID := int64(0)
				for i, row := range q.rows {
					if !reflect.DeepEqual(row.rec, want[i]) || row.id <= lastID {
						t.Fatalf("retry record/order %d differs: id=%d after=%d", i, row.id, lastID)
					}
					lastID = row.id
					keys = append(keys, row.rec.PartitionKey)
					pending, err := q.Pending(ctx, row.rec.PartitionKey, len(want)+1)
					if err != nil || len(pending) != 1 || pending[0].ID != row.id || !reflect.DeepEqual(pending[0].Record, want[i]) {
						t.Fatalf("visible retry record %d = %v, %v", i, pending, err)
					}
				}
				got, err := q.PendingKeys(ctx, len(want)+1)
				if err != nil || !reflect.DeepEqual(got, keys) {
					t.Fatalf("retry keys = %v, %v; want %v", got, err, keys)
				}
				for i := range before {
					if !reflect.DeepEqual(*q.rows[i], before[i]) {
						t.Fatalf("valid retry changed preexisting metadata %d", i)
					}
				}
			})
		}
	}
}
