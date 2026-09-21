package position

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveryProjectorReordersAndAcknowledgesOnceOverRealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires PostgreSQL and JetStream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "position-recovery-" + suffix
	pool := newPool(t, tenant)
	freshSchema(t, pool)
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_POSITION_"+suffix, []string{"order.>"})
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_RISK_"+suffix, []string{"risk.position.>"})
	stream, err := bustest.StreamFor(ctx, js, fillfact.SubjectRecovered)
	if err != nil {
		t.Fatal(err)
	}
	pull := func(name, subject string) jetstream.Consumer {
		c, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name + suffix, FilterSubject: subject, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, name+suffix) })
		return c
	}
	input, acks := pull("position-input-", fillfact.SubjectRecovered), pull("position-ack-", fillfact.RecoveryPositionApplied)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-recovery-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	ctx = bus.WithTenantID(ctx, tenant)
	start := time.Unix(1700000000, 0)
	first := buy("first", "BTC-USD", "1", "100", start)
	last := fillAt("last", "BTC-USD", "1", "150", orderpb.Side_SIDE_SELL, start.Add(2*time.Second))
	last.Venue = first.Venue
	late := buy("late", "BTC-USD", "1", "200", start.Add(time.Second))
	for i, fill := range []*orderpb.Fill{first, last, late} {
		fill.OrderId = "order-" + fill.FillId
		fill.VenueAccountId = "account"
		fill.VenueExecutionId = fill.FillId
		fill.Fee = &commonpb.Money{Amount: &commonpb.Decimal{}, CurrencyCode: "USD"}
		digest, err := fillfact.ExecutionDigest(fill)
		if err != nil {
			t.Fatal(err)
		}
		fill.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case-" + fill.FillId, MappingVersion: "version", SourceCursor: "cursor", PayloadDigest: strings.Repeat("a", 64), ExecutionDigest: digest, ObservedAt: timestamppb.Now()}
		fact := &orderpb.ExecutionRecovered{Fill: fill, State: &orderpb.OrderState{OrderId: fill.OrderId, PortfolioId: "fund", Venue: fill.Venue, VenueAccountId: fill.VenueAccountId, InstrumentId: fill.InstrumentId, Side: fill.Side, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}}
		if err := producer.Publish(ctx, bus.Event{Subject: fillfact.SubjectRecovered, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", PartitionKey: fill.OrderId, EventTime: time.Now(), Payload: fact}); err != nil {
			t.Fatal(err)
		}
		for delivery := 0; delivery < 2; delivery++ {
			msg, err := input.Next(jetstream.FetchMaxWait(5 * time.Second))
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
			projector, err := NewProjector(NewPostgres(pool, "USD"), producer, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if err := projector.Handle(ctx, env, payload); err != nil {
				t.Fatal(err)
			}
			if delivery == 0 {
				if err := msg.Nak(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
			}
			ackMsg, err := acks.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			ackEnv, ackPayload, err := bus.Unframe(ackMsg.Data())
			if err != nil {
				t.Fatal(err)
			}
			var ack orderpb.ExecutionRecoveryApplied
			if err := proto.Unmarshal(ackPayload, &ack); err != nil || ack.ExecutionDigest != digest || ack.CaseId != fill.Recovery.CaseId || ackEnv.GetTenantId() != tenant {
				t.Fatalf("ack=%v err=%v", &ack, err)
			}
			if err := ackMsg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var claims int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM position_fills`).Scan(&claims); err != nil || claims != i+1 {
			t.Fatalf("claims=%d err=%v", claims, err)
		}
	}
	snapshot, err := NewPostgres(pool, "USD").Snapshot(ctx, "fund", start.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Positions) != 1 {
		t.Fatalf("positions=%v", snapshot.Positions)
	}
	position := snapshot.Positions[0]
	if dec.FromProto(position.Quantity).Cmp(big.NewRat(1, 1)) != 0 || dec.FromProto(position.AveragePrice).Cmp(big.NewRat(150, 1)) != 0 || dec.FromProto(position.RealizedPnl.Amount).Sign() != 0 {
		t.Fatalf("arrival-order economics persisted: %v", position)
	}
}
