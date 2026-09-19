package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestVenueDiscrepancyRealBrokerDurableReplay(t *testing.T) {
	for _, subject := range []string{execution.SubjectBalanceRecon, execution.SubjectStateHealed} {
		t.Run(subject, func(t *testing.T) { realDiscrepancyReplay(t, subject) })
	}
}

func realDiscrepancyReplay(t *testing.T, subject string) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream")
	}
	pool := newAuditPool(t)
	var privileged bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("restricted role required: %v %v", privileged, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("VENUE_DISCREPANCY_%d", time.Now().UnixNano())
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = js.StreamNameBySubject(ctx, subject); err != nil {
		if _, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{subject}, Storage: jetstream.FileStorage}); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = js.DeleteStream(context.Background(), name) }()
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	raw, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", ProducerVersion: "test", Tenant: "tenant-A"})
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	producer := execution.NewDiscrepancyPublisher(raw, registry)
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	var observed atomic.Int32
	projector := NewProjector(NewPostgres(pool), time.Now, WithDiscrepancyObserver(func(kind, result string) {
		if result == "observed" {
			observed.Add(1)
		}
	}))
	received := make(chan struct{}, 8)
	done := make(chan error, 1)
	var lostAck atomic.Bool
	go func() {
		done <- consumer.Subscribe(ctx, subject, name, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.CorrelationId != name {
				return nil
			}
			if err := projector.Handle(ctx, env, payload); err != nil {
				return err
			}
			// The DB committed but the delivery did not acknowledge: the actual broker
			// must redeliver without producing a second immutable audit row.
			if !lostAck.Swap(true) {
				return errors.New("simulated interruption after commit")
			}
			select {
			case received <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("subscriber failed to stop")
		}
	}()
	for i, tenant := range []string{"tenant-A", "tenant-A", "tenant-B"} {
		msg := discrepancyBalance()
		// Reversed domain times remain separate observations, never overwrite or
		// resolve an earlier discrepancy merely because delivery order changed.
		msg.DetectedAt = timestamppb.New(time.Now().Add(-time.Duration(i) * time.Hour))
		event := bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", EventTime: msg.DetectedAt.AsTime(), PartitionKey: tenant + ":USD", TenantID: tenant, CorrelationID: name, PayloadSchemaRef: "accounting.v1.BalanceReconciled:1", Payload: msg}
		if subject == execution.SubjectStateHealed {
			event.Domain = "order"
			event.PayloadSchemaRef = "order.v1.StateHealed:1"
			event.Payload = &orderpb.StateHealed{OrderId: "terminal-order", Venue: "BINANCE", DetectedAt: msg.DetectedAt,
				State: &orderpb.OrderState{OrderId: "terminal-order", Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, FilledQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: -9}}}
		}
		if err = producer.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatal("durable delivery missing")
		}
	}
	if observed.Load() != 4 {
		t.Fatalf("expected four committed deliveries including redelivery, got %d", observed.Load())
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var published float64
	for _, family := range families {
		if family.GetName() == "kanz_venue_discrepancy_publish_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "result" && label.GetValue() == "published" {
						published += metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	if published != 3 {
		t.Fatalf("real broker publish counter = %v, want 3", published)
	}
	freshPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer freshPool.Close()
	fresh := NewPostgres(freshPool)
	rows, err := fresh.Query(ctx, Filter{Tenant: "tenant-A", Kind: KindVenueDiscrepancy, Correlation: name, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("duplicate or tenant leak after reconnect: %d %v", len(rows), err)
	}
	if !rows[1].OccurredAt.Before(rows[0].OccurredAt) {
		t.Fatal("observation chronology was rewritten")
	}
	for _, r := range rows {
		quantity := r.Attributes["actual"]
		if subject == execution.SubjectStateHealed {
			quantity = r.Attributes["filled_quantity"]
		}
		if quantity != "1/1000000000" || r.Attributes["disposition"] != "investigate" {
			t.Fatalf("wrong evidence: %+v", r)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE audit_log SET summary='repaired'`); err == nil {
		t.Fatal("immutable evidence was mutable")
	}
}
