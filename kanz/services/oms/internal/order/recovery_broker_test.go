package order

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// This proves durable investigation evidence and real transport redelivery. It
// deliberately makes no claim about the separate economic posting boundary.
func TestRecoveryEvidenceOutboxSurvivesRestartAndBrokerRedelivery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL and TEST_POSTGRES_URL for real recovery evidence proof")
	}
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(context.Background(), testTenant), 30*time.Second)
	defer cancel()
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("recovery proof requires a restricted PostgreSQL role")
	}
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
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_EVIDENCE_"+suffix, []string{"order.>"})
	stream, err := bustest.StreamFor(ctx, js, EventTypeRecoveryRecorded)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: "recovery-proof-" + suffix,
		FilterSubject: EventTypeRecoveryRecorded, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, "recovery-proof-"+suffix) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "recovery-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	s := NewPostgres(pool)
	id := "case-" + suffix
	c, err := s.ObserveRecovery(ctx, id, "order-"+suffix, "source-"+suffix, []byte("immutable discrepancy"))
	if err != nil {
		t.Fatal(err)
	}
	st := &orderpb.OrderState{OrderId: c.OrderID, PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
	if err := s.BlockRecovery(ctx, c, st, "verified account mapping unavailable", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart after the database commit and before publication.
	restarted := NewPostgres(pool)
	relay, err := outbox.NewRelay(restarted.Outbox(), producer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := relay.Flush(ctx, c.OrderID); err != nil || count != 1 {
		t.Fatalf("drain=%d err=%v", count, err)
	}
	var firstSequence uint64
	for delivery := 0; delivery < 2; delivery++ {
		msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var fact orderpb.ExecutionRecoveryRecorded
		if err := proto.Unmarshal(payload, &fact); err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() != testTenant || fact.CaseId != id || fact.Status != "blocked" || fact.PayloadDigest != c.Digest {
			t.Fatalf("fact=%v envelope=%v", &fact, env)
		}
		metadata, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if delivery == 0 {
			firstSequence = metadata.Sequence.Stream
			if err := msg.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if metadata.Sequence.Stream != firstSequence || metadata.NumDelivered < 2 {
				t.Fatal("did not receive a broker redelivery")
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	again, err := restarted.ObserveRecovery(ctx, c.ID, c.OrderID, c.SourceCursor, c.Evidence)
	if err != nil || again.Status != "blocked" || again.Version != 1 {
		t.Fatalf("redelivery changed durable case: %+v %v", again, err)
	}
	if count, err := relay.Flush(ctx, c.OrderID); err != nil || count != 0 {
		t.Fatalf("published again: %d %v", count, err)
	}
}
