package outbox

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// The foreign relay uses a real producer and broker. If its queue selects an
// OMS row, it can publish a valid envelope stamped with the wrong source; a
// recorder would miss that institutional provenance failure (#1325).
func TestPostgresOutboxRejectsCrossServiceOwnership(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL and TEST_POSTGRES_URL for outbox ownership proof")
	}
	pool := newPool(t, testTenant)
	requireRestrictedRole(t, pool)
	applySchema(t)
	ctx, cancel := context.WithTimeout(testCtx(), 45*time.Second)
	defer cancel()
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	const subject = "order.order.cancelled"
	bustest.EnsureSubjects(t, ctx, js, "OUTBOX_OWNER_"+suffix, []string{subject})
	stream, err := bustest.StreamFor(ctx, js, subject)
	if err != nil {
		t.Fatal(err)
	}
	consumerName := "outbox-owner-" + suffix
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Name: consumerName, FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, consumerName) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "outbox-owner-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	key := "outbox-owner-" + suffix
	var records []Record
	for sequence := int64(1); sequence <= 2; sequence++ {
		r, err := From(ctx, bus.Event{
			Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "order", EventTime: time.Now(), PartitionKey: key,
			Payload: &orderpb.OrderCancelled{OrderId: key, CancelledQuantity: &commonpb.Decimal{Coefficient: sequence}},
		})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	enqueueOwned(t, pool, ServiceOMS, records...)
	makeRelay := func(q Queue, source string) *Relay {
		t.Helper()
		producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: source, ProducerVersion: "test", Tenant: testTenant})
		if err != nil {
			t.Fatal(err)
		}
		relay, err := NewRelay(q, producer, quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		return relay
	}
	foreign := makeRelay(NewPostgres(pool, ServiceAccounting), "accounting/foreign-relay")
	if n, err := foreign.Flush(ctx, key); err != nil || n != 0 {
		t.Fatalf("foreign relay published OMS facts: n=%d err=%v", n, err)
	}
	// Fresh queue objects model replicas restarting after the transaction but
	// before either publication. Both wait for the same service/key lock.
	replicas := []*Relay{
		makeRelay(NewPostgres(pool, ServiceOMS), "oms/replica1"),
		makeRelay(NewPostgres(newPool(t, testTenant), ServiceOMS), "oms/replica2"),
	}
	type result struct {
		n   int
		err error
	}
	results := make(chan result, len(replicas))
	for _, relay := range replicas {
		go func() {
			n, err := relay.Flush(ctx, key)
			results <- result{n, err}
		}()
	}
	total := 0
	var failures []error
	for range replicas {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err)
		}
		total += r.n
	}
	if len(failures) != 0 {
		t.Fatalf("replica drains: %v", failures)
	}
	if total != len(records) {
		t.Fatalf("replicas published %d facts, want %d", total, len(records))
	}
	for want := int64(1); want <= 2; {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if env.GetPartitionKey() != key {
			_ = msg.Ack()
			continue
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var observed orderpb.OrderCancelled
		if err := proto.Unmarshal(payload, &observed); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(env.GetSource(), "oms/") || env.GetTenantId() != testTenant ||
			observed.GetCancelledQuantity().GetCoefficient() != want {
			t.Fatalf("wrong provenance or ordering: source=%s tenant=%s quantity=%v want=%d",
				env.GetSource(), env.GetTenantId(), observed.GetCancelledQuantity(), want)
		}
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		want++
	}
	if n, err := makeRelay(NewPostgres(pool, ServiceOMS), "oms/restarted").Flush(ctx, key); err != nil || n != 0 {
		t.Fatalf("restart republished durable facts: n=%d err=%v", n, err)
	}
}
