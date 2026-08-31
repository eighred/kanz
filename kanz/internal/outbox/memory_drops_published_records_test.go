package outbox

// THE IN-PROCESS QUEUE MUST NOT GROW WITH THE PROCESS'S LIFETIME VOLUME (#890).
//
// Memory.rows was append-only: MarkPublished flipped a bool and every read
// filtered on it, so a *memRow and its marshalled payload survived for the life
// of the process — one per order accepted, per fill folded, per ledger posting.
// On the no-DSN posture that is the OMS's own heap growing with trading volume,
// reported by nothing until the pod is OOM-killed.
//
// These read len/cap of the unexported field ON PURPOSE. The bound is a property
// of the storage, and PendingCount alone cannot see it: the leaking version
// answered PendingCount correctly on every one of the rows it was hoarding.

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// A LONG RUN LEAVES NOTHING BEHIND, and the capacity is the half that says so.
//
// len(rows)==0 would also hold for a queue that reslices without ever releasing
// its backing array; cap is what proves the heap the queue holds is bounded by
// the PENDING depth (one record at a time here) rather than by the 2000 records
// that have been through it.
func TestMemoryDropsPublishedRecordsSoALongRunStaysBounded(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	rec := &recorder{}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	const orders = 2000
	for i := range orders {
		key := "o" + strconv.Itoa(i)
		mustAppend(t, q, fact(t, ctx, "order.order.accepted", key))
		sent, err := relay.Flush(ctx, key)
		if err != nil || sent != 1 {
			t.Fatalf("Flush(%s) = (%d, %v), want (1, nil)", key, sent, err)
		}
	}

	if got := len(rec.types()); got != orders {
		t.Fatalf("the relay published %d FACTs for %d orders — the bound must not be reached by "+
			"dropping records instead of sending them", got, orders)
	}
	if got := len(q.rows); got != 0 {
		t.Errorf("rows holds %d record(s) after %d orders were published and nothing is pending — "+
			"a published record is retained by nothing and reachable by nothing, so this is the OMS's "+
			"heap growing with trading volume until the pod is OOM-killed (#890)", got, orders)
	}
	if got := q.PendingCount(); got != 0 {
		t.Errorf("PendingCount = %d after every record published, want 0", got)
	}
	// The peak pending depth in this loop is one record, so one slot is the
	// honest ceiling; the slack allows for append's growth policy without
	// admitting anything that scales with `orders`.
	if got := cap(q.rows); got > 8 {
		t.Errorf("rows retains a backing array of %d slot(s) after %d orders whose peak pending depth "+
			"was 1 — the queue is still holding memory in proportion to what it has published rather "+
			"than to what it owes", got, orders)
	}
	// And the slots ABOVE len must not point at anything either: a reslice that
	// leaves the pointer behind keeps that record's marshalled payload reachable
	// until an Append happens to overwrite the slot, which on a queue that has
	// gone quiet is never.
	retained, sample := 0, ""
	for _, r := range q.rows[:cap(q.rows)] {
		if r != nil {
			retained++
			sample = r.rec.EventType
		}
	}
	if retained > 0 {
		t.Errorf("%d slot(s) of the backing array still point at drained records (e.g. a %s) — the rows "+
			"are out of the queue and their payloads are still on the heap", retained, sample)
	}
	if age, ok, err := q.OldestPendingAge(ctx, time.Now()); err != nil || ok || age != 0 {
		t.Errorf("OldestPendingAge = (%v, %v, %v) on a fully drained queue, want (0, false, nil) — "+
			"the gauge would report a backlog made entirely of records the broker already has",
			age, ok, err)
	}
}

// A RECORD THE BROKER DID NOT TAKE MUST STILL BE THERE.
//
// This is the half of the bound that cannot be traded away: the drop is
// downstream of a successful Publish, never ahead of it. An eviction that ran on
// a failed attempt — or on a timer, or on a cap — would delete a committed FACT
// that was never announced, which is the exact defect the outbox package exists
// to remove, reintroduced through its memory footprint.
func TestMemoryKeepsARecordWhosePublishFailedSoTheRetryStillHasIt(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	rec := &recorder{failOn: "order.order.accepted"}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))

	if sent, stall := relay.Flush(ctx, "o1"); sent != 0 || stall == nil {
		t.Fatalf("Flush = (%d, %v) while the broker was refusing, want (0, a stall)", sent, stall)
	}
	if got := len(q.rows); got != 1 {
		t.Fatalf("rows holds %d record(s) after a REFUSED publish, want 1 — the FACT is committed and "+
			"was never announced, and dropping it here is the loss the outbox exists to prevent", got)
	}
	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 {
		t.Fatalf("Pending after a refusal = (%d records, %v), want the record back with attempts=1",
			len(pending), err)
	}
	if pending[0].LastError == "" {
		t.Error("the retained record carries no LastError — the cause is what an operator reads next (#817)")
	}

	rec.setFailOn("")
	sent, err := relay.DrainOnce(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("DrainOnce after the broker recovered = (%d, %v), want (1, nil)", sent, err)
	}
	if got := len(q.rows); got != 0 {
		t.Errorf("rows holds %d record(s) after the retry succeeded, want 0", got)
	}
}

// REMOVAL MUST NOT REORDER THE RECORDS AROUND IT, AND THIS IS THE ASSERTION THE
// FIX NEEDED MOST.
//
// The obvious cheap removal is the swap-remove — move the last element over the
// hole and truncate — and it SURVIVED every other test in this package when it
// was tried as a mutation. One slice holds every key's records, so lifting the
// tail into a hole in the middle silently reorders a DIFFERENT order's backlog:
// two orders interleaved, drain the first, and the second's ROUTED now sits
// ahead of its ACCEPTED. tv-sync's transition() DROPS a routed FACT for an order
// it never admitted, so the projection goes permanently blind to a live order
// while the OMS believes it announced everything. That is the one failure mode
// Queue's ordering contract exists to prevent, and it would have been introduced
// by a memory fix.
func TestMemoryDroppingARecordLeavesTheOthersInEnqueueOrder(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	rec := &recorder{}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	// INTERLEAVED on purpose: the removals are then from the middle of a slice
	// two orders share, which is the only arrangement that can expose this.
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o2"))
	mustAppend(t, q, fact(t, ctx, "order.order.routed", "o1"))
	mustAppend(t, q, fact(t, ctx, "order.order.routed", "o2"))

	if sent, err := relay.Flush(ctx, "o1"); err != nil || sent != 2 {
		t.Fatalf("Flush(o1) = (%d, %v), want (2, nil)", sent, err)
	}

	pending, err := q.Pending(ctx, "o2", 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending(o2) after o1 drained = (%d records, %v), want (2, nil)", len(pending), err)
	}
	if pending[0].ID > pending[1].ID {
		t.Errorf("o2's backlog came back as ids %d then %d after o1's records were removed — dropping "+
			"a record reordered an untouched order's queue, and the relay publishes in this order",
			pending[0].ID, pending[1].ID)
	}

	if sent, err := relay.DrainOnce(ctx); err != nil || sent != 2 {
		t.Fatalf("DrainOnce = (%d, %v), want (2, nil)", sent, err)
	}
	want := []string{
		"order.order.accepted", "order.order.routed", // o1, from the Flush
		"order.order.accepted", "order.order.routed", // o2, from the pass after it
	}
	got := rec.types()
	if len(got) != len(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published %v, want %v — a ROUTED ahead of its own ACCEPTED leaves tv-sync's "+
				"projection permanently blind to a live order", got, want)
		}
	}
}

// A DROPPED RECORD MUST NOT COME BACK, and a late mark for it must be silence.
//
// Postgres answers both of these with `AND published_at IS NULL` and treats
// RowsAffected()==0 as success; it cannot tell a published id from one that never
// existed and does not need to. This queue must land in the same place, or a
// second relay pass — or a MarkFailed racing a MarkPublished — would republish a
// FACT consumers have already folded, or resurrect it into the pending set where
// it would block every FACT behind it on that key.
func TestMemoryTreatsAMarkForADroppedRecordAsSilence(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	rec := &recorder{}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = (%d, %v)", len(pending), err)
	}
	id := pending[0].ID

	if sent, err := relay.DrainOnce(ctx); err != nil || sent != 1 {
		t.Fatalf("DrainOnce = (%d, %v), want (1, nil)", sent, err)
	}
	if err := q.MarkFailed(ctx, id, errors.New("a mark from a pass that had already lost the key")); err != nil {
		t.Fatalf("MarkFailed for a drained record: %v", err)
	}
	if err := q.MarkPublished(ctx, id); err != nil {
		t.Fatalf("MarkPublished for a drained record: %v", err)
	}
	if got := len(q.rows); got != 0 {
		t.Fatalf("a mark for a drained record put %d row(s) back in the queue — it would be published a "+
			"second time and, until it was, would block every FACT behind it on this key", got)
	}
	if sent, err := relay.DrainOnce(ctx); err != nil || sent != 0 {
		t.Fatalf("a second pass published %d record(s) (err %v) — the drop is not sticking", sent, err)
	}
	if got := len(rec.types()); got != 1 {
		t.Errorf("the FACT went out %d times, want exactly once", got)
	}
}
