package collateralops

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// A real JetStream broker transports source snapshots and committed outbox
// revisions. No in-memory bus can prove either durable boundary.
func TestPostgresJetStreamCollateralRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream via TEST_NATS_URL")
	}
	s, _ := database(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	subject := "test.collateral." + suffix
	stream := "COLLATERAL_TEST_" + suffix
	if _, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: stream, Subjects: []string{subject + ".>"}, Storage: jetstream.FileStorage}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "collateral-integration"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	feed, err := bus.NewProducer(client, bus.ProducerConfig{Source: "trusted-feed", ProducerVersion: "test", Tenant: "tenant-A"})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewConsumer(s, "tenant-A", "trusted-feed", `[{"Source":"trusted-custody","Custodian":"custodian","Account":"account"}]`)
	if err != nil {
		t.Fatal(err)
	}
	ingested := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, subject+".input", "input-"+suffix, func(ctx context.Context, e *envelopepb.Envelope, b []byte) error {
			err := handler.HandleSnapshot(ctx, e, b)
			select {
			case ingested <- err:
			default:
			}
			return err
		})
	}()
	input := fixture()
	if err = feed.Publish(ctx, bus.Event{Subject: subject + ".input", EventType: SubjectSnapshot, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", EventTime: input.AsOf.AsTime(), PartitionKey: input.PortfolioId, PayloadSchemaRef: "collateral.v1.WorkflowSnapshot:1", Payload: input}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ingested:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatalf("subscription exited: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state, err := s.Apply(ctx, Action{Tenant: "tenant-A", Actor: "maker", RequestID: "request", WorkflowID: "workflow-1", SnapshotID: input.SnapshotId, Kind: "propose"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep a uniquely scoped test stream; production records retain the logical
	// subject. This test routes the queued row through the real Relay/Producer.
	if _, err = s.pool.Exec(ctx, `UPDATE outbox SET subject=$1 WHERE partition_key=$2`, subject+".recorded", state.WorkflowId); err != nil {
		t.Fatal(err)
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: "tenant-A"})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(outbox.NewPostgres(s.pool, "accounting"), producer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("relay %d: %v", n, err)
	}
	received := make(chan *pb.WorkflowRecorded, 1)
	go func() {
		_ = consumer.Subscribe(ctx, subject+".recorded", "recorded-"+suffix, func(_ context.Context, e *envelopepb.Envelope, b []byte) error {
			if e.TenantId != "tenant-A" || e.Source != "accounting" {
				return ErrInput
			}
			record := new(pb.WorkflowRecorded)
			if err := proto.Unmarshal(b, record); err != nil {
				return err
			}
			select {
			case received <- record:
			default:
			}
			return nil
		})
	}()
	select {
	case got := <-received:
		if !proto.Equal(got, state) {
			t.Fatal("outbox changed recorded state")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state = act(t, s, state, "checker", "approve")
	custodyProducer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "trusted-custody", ProducerVersion: "test", Tenant: "tenant-A"})
	if err != nil {
		t.Fatal(err)
	}
	custodyHandler := handler.ConfirmationHandlers()[SubjectConfirmation+".trusted-custody"]
	confirmationDone := make(chan error, 1)
	confirmationReceived := make(chan error, 2)
	go func() {
		confirmationDone <- consumer.Subscribe(ctx, subject+".custody", "custody-"+suffix, func(ctx context.Context, e *envelopepb.Envelope, b []byte) error {
			err := custodyHandler(ctx, e, b)
			select {
			case confirmationReceived <- err:
			default:
			}
			return err
		})
	}()
	confirmation := confirmed(state, "custody-settled", 60, false)
	for range 2 {
		if err = custodyProducer.Publish(ctx, bus.Event{Subject: subject + ".custody", EventType: SubjectConfirmation, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", EventTime: confirmation.SettledAt.AsTime(), PartitionKey: state.WorkflowId, PayloadSchemaRef: "collateral.v1.PostingConfirmation:1", Payload: confirmation}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-confirmationReceived:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if got := reload(t, s); got.Status != pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED || got.Revision != 3 {
		t.Fatalf("broker settlement/replay: %+v", got)
	}
	cancel()
	select {
	case <-confirmationDone:
	case <-time.After(8 * time.Second):
		t.Fatal("custody subscription did not drain")
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("subscription did not drain")
	}
}
