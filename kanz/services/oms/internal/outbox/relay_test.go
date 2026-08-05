package outbox

// Relay tests over the in-process Queue.
//
// WHAT THESE CAN AND CANNOT PROVE. They run the real Relay against the real
// Memory queue, so the ORDERING logic — per-key sequence, head-of-line on a
// failed publish, one drainer per key — is genuinely exercised. What they cannot
// prove is the property the whole package exists for: that the state change and
// the outbox record COMMIT TOGETHER. That is a transaction, and a map behind a
// mutex is not one. postgres_test.go carries that assertion, gated on
// TEST_POSTGRES_URL.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

const testTenant = "acme"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder is the publish side. failOn makes ONE event type refuse, which is how
// a broker blip mid-sequence is modelled.
type recorder struct {
	mu     sync.Mutex
	got    []bus.Event
	failOn string
	// beforePublish runs inside Publish, so a test can interleave a second
	// drainer at the exact moment one holds the key.
	beforePublish func()
}

func (r *recorder) Publish(_ context.Context, e bus.Event) error {
	if r.beforePublish != nil {
		r.beforePublish()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn != "" && e.EventType == r.failOn {
		return errors.New("recorder: injected publish failure for " + r.failOn)
	}
	r.got = append(r.got, e)
	return nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.got))
	for i, e := range r.got {
		out[i] = e.EventType
	}
	return out
}

func (r *recorder) setFailOn(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn = s
}

// fact builds a record the way order.Emitter does, through From, so these tests
// exercise the real capture path rather than a hand-filled struct.
func fact(t *testing.T, ctx context.Context, eventType, orderID string) Record {
	t.Helper()
	rec, err := From(ctx, bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "order",
		EventTime:        time.Unix(0, 0).UTC(),
		PartitionKey:     orderID,
		PayloadSchemaRef: "order.v1.OrderCancelled:1",
		Payload: &orderpb.OrderCancelled{
			OrderId:           orderID,
			CancelledQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		},
	})
	if err != nil {
		t.Fatalf("From(%s): %v", eventType, err)
	}
	// The event type is what the assertions read; the payload is one message
	// type throughout because these tests are about SEQUENCE, not content.
	rec.EventType = eventType
	rec.Subject = eventType
	return rec
}

func testCtx() context.Context {
	return bus.WithTenantID(context.Background(), testTenant)
}

// FROM REFUSES A RECORD WITH NO TENANT, and it must, because bus.Validate
// rejects an empty tenant_id on the live path: an untenanted record can never be
// published, so it would sit at the head of its key blocking every FACT behind
// it forever. Refusing at capture means the state change rolls back with it.
func TestFromRefusesAnEventWithNoTenant(t *testing.T) {
	_, err := From(context.Background(), bus.Event{
		EventType:    "order.order.cancelled",
		EventTime:    time.Unix(0, 0),
		PartitionKey: "o1",
		Payload:      &orderpb.OrderCancelled{OrderId: "o1"},
	})
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("From with no tenant returned %v, want ErrNoTenant", err)
	}
}

// A RECORD MUST CARRY THE LINEAGE ITS SYNCHRONOUS PUBLISH WOULD HAVE INHERITED.
//
// correlation_id, causation_id and the tenant come off the inbound envelope onto
// ctx (bus.Consumer does this per delivery). The relay publishes from a ticker,
// with none of that on its own ctx — so if the record does not carry them, the
// FACT publishes fine, validates fine, and silently stops being traceable to the
// command that caused it. Nothing fails; that is why this is asserted.
func TestRecordCarriesLineageThroughTheRelay(t *testing.T) {
	ctx := bus.WithCausationID(bus.WithCorrelationID(testCtx(), "corr-1"), "cause-1")
	rec := fact(t, ctx, "order.order.cancelled", "o1")

	got, err := rec.Event()
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	if got.CorrelationID != "corr-1" || got.CausationID != "cause-1" || got.TenantID != testTenant {
		t.Fatalf("lineage lost through the outbox: correlation=%q causation=%q tenant=%q",
			got.CorrelationID, got.CausationID, got.TenantID)
	}
	if got.Payload == nil {
		t.Fatal("payload did not re-inflate")
	}
	if _, ok := got.Payload.(*orderpb.OrderCancelled); !ok {
		t.Fatalf("payload re-inflated as %T, want *orderpb.OrderCancelled — the concrete type is "+
			"resolved from payload_schema_ref, so a wrong one means the ref no longer names the message", got.Payload)
	}
}

// AN UNRESOLVABLE PAYLOAD TYPE IS A HEAD-OF-LINE STOP, NOT A SKIP. A relay
// running older code than the FACTs it is asked to send must not publish the
// events AROUND the one it cannot decode — that hands a consumer a history with
// a hole in it.
func TestRecordWithAnUnknownPayloadTypeRefuses(t *testing.T) {
	rec := fact(t, testCtx(), "order.order.cancelled", "o1")
	rec.PayloadSchemaRef = "order.v1.NoSuchMessageEverRegistered:1"
	if _, err := rec.Event(); !errors.Is(err, ErrUnknownPayloadType) {
		t.Fatalf("Event with an unknown ref returned %v, want ErrUnknownPayloadType", err)
	}
}

// THE ORDERING GUARANTEE, ASSERTED DIRECTLY: records for one key publish in the
// order they were enqueued. This is the property a consumer folds on — tv-sync's
// transition() DROPS a routed FACT for an order it never admitted — so a relay
// that reordered would not make a projection late, it would make it permanently
// wrong.
func TestRelayPublishesOneKeyInEnqueueOrder(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	want := []string{"order.order.accepted", "order.order.routed", "order.order.filled"}
	for _, ty := range want {
		if err := q.Append(fact(t, ctx, ty, "o1")); err != nil {
			t.Fatalf("Append %s: %v", ty, err)
		}
	}
	rec := &recorder{}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 3 {
		t.Fatalf("DrainOnce = (%d, %v), want (3, nil)", n, err)
	}
	got := rec.types()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published %v, want %v — a FACT out of sequence for one order is not a delay, "+
				"it is a projection folded against a history that never happened", got, want)
		}
	}
	if q.PendingCount() != 0 {
		t.Fatalf("%d records still pending after a clean drain", q.PendingCount())
	}
}

// A FAILED PUBLISH HOLDS EVERY FACT BEHIND IT ON THAT ORDER, AND ONLY THAT
// ORDER.
//
// Skipping the stuck record and carrying on would publish this order's history
// out of sequence — the failure the ordering contract exists to prevent — so the
// key blocks. Other keys must be untouched by it, which is the entire reason the
// lock is per key rather than per table.
func TestRelayStopsAtAFailedRecordButDrainsOtherKeys(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	mustAppend(t, q, fact(t, ctx, "order.order.routed", "o1"))
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o2"))

	rec := &recorder{failOn: "order.order.accepted"}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	// o1's head fails, so o1's routed must NOT go out. o2's head fails too, so
	// nothing publishes at all — but the pass must not abort on the first stuck
	// key, or one poison order shadows the whole book.
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("DrainOnce = (%d, %v), want (0, nil)", n, err)
	}
	if got := rec.types(); len(got) != 0 {
		t.Fatalf("published %v while both heads were failing, want nothing", got)
	}

	// o2 recovers; o1 does not (its head is still the failing type... so make
	// only o1's head fail by letting accepted through and failing routed).
	rec.setFailOn("order.order.routed")
	if _, err := relay.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce after recovery: %v", err)
	}
	got := rec.types()
	if len(got) != 2 {
		t.Fatalf("published %v, want both accepted FACTs and neither routed", got)
	}
	if q.PendingCount() != 1 {
		t.Fatalf("%d records pending, want 1 — o1's routed is held behind nothing at all if this is 0", q.PendingCount())
	}

	// FLUSH REPORTS THE STALL. The background pass tolerates it; a handler about
	// to publish the next FACT for this order directly must not.
	if sent, err := relay.Flush(ctx, "o1"); err == nil {
		t.Fatalf("Flush = (%d, nil) for a key that still holds an unpublished record — its caller "+
			"would go on to publish the NEXT FACT for this order directly, ahead of this one", sent)
	}
	// AND THE COUNT IS THE ONE A CALLER ACTS ON. completeTerminalOutcome decides
	// whether an interrupted fill was RECOVERED or LOST on this number, so a Flush
	// that drained nothing must not report that it did.
	if sent, err := relay.Flush(ctx, "o2"); err != nil || sent != 0 {
		t.Fatalf("Flush on an already-drained key = (%d, %v), want (0, nil) — a caller reading a "+
			"non-zero count here would report a FACT recovered that was published a pass ago", sent, err)
	}
}

// TWO RELAYS, ONE KEY: exactly one drains it.
//
// oms-deploy.yaml runs replicas: 2, so this is the normal case, not an edge one.
// `SELECT ... FOR UPDATE SKIP LOCKED` — the usual answer — would hand the second
// relay the next UNLOCKED ROW, which for a key whose head is already locked is a
// LATER ROW OF THE SAME KEY: the two would publish that order out of order, and
// no single-relay test would ever show it. The lock is on the KEY for exactly
// that reason, and this pins it.
func TestTwoRelaysDoNotInterleaveOneKey(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	mustAppend(t, q, fact(t, ctx, "order.order.routed", "o1"))

	// A PLAIN FLAG, NOT sync.Once. The nested drain below runs INSIDE Publish, so
	// if the lock ever stops excluding it the second relay publishes too — and its
	// publish re-enters this hook. sync.Once.Do re-entered from inside its own f
	// deadlocks, which would turn a broken lock into a hung test instead of a
	// failed assertion. A hang is the worst failure signal there is: it looks like
	// a slow machine.
	var nestedOnce bool
	rec := &recorder{}
	relayA, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	relayB, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	// While A is inside the publish of o1's FIRST record — key locked, second
	// record still unpublished — B runs a full background pass. If B could see
	// past the lock it would publish o1's routed here, ahead of the accepted A
	// is still sending.
	rec.beforePublish = func() {
		if nestedOnce {
			return
		}
		nestedOnce = true
		if n, err := relayB.DrainOnce(ctx); err != nil || n != 0 {
			t.Errorf("the second relay published %d records for a key the first holds (err %v); "+
				"that is the SKIP LOCKED reordering this lock exists to prevent", n, err)
		}
	}
	if n, err := relayA.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("relay A DrainOnce = (%d, %v), want (2, nil)", n, err)
	}
	want := []string{"order.order.accepted", "order.order.routed"}
	got := rec.types()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published %v, want %v exactly once each and in order", got, want)
	}
}

// THE OLDEST-PENDING AGE IS THE ONLY SIGNAL THAT SEPARATES A DRAINED OUTBOX FROM
// A RELAY THAT DIED. Both produce zero errors, zero failed publishes and a
// silent log.
func TestOldestPendingAgeDistinguishesEmptyFromStuck(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	now := time.Unix(1_700_000_000, 0).UTC()
	q.nowFunc = func() time.Time { return now }

	if _, ok, err := q.OldestPendingAge(ctx, now); err != nil || ok {
		t.Fatalf("empty queue reported ok=%v err=%v, want ok=false", ok, err)
	}
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	age, ok, err := q.OldestPendingAge(ctx, now.Add(90*time.Second))
	if err != nil || !ok || age != 90*time.Second {
		t.Fatalf("OldestPendingAge = (%v, %v, %v), want (90s, true, nil)", age, ok, err)
	}
}

func mustAppend(t *testing.T, q *Memory, r Record) {
	t.Helper()
	if err := q.Append(r); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
