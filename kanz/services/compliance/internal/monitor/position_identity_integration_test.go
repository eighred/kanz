package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type positionIdentityPublisher struct {
	producer *bus.Producer
	calls    atomic.Int64
}

func (p *positionIdentityPublisher) Publish(ctx context.Context, event bus.Event) error {
	p.calls.Add(1)
	return p.producer.Publish(ctx, event)
}

type positionIdentityDelivery struct {
	env     *envelopepb.Envelope
	payload []byte
	err     error
}

// Production uses plain broadcast: handler errors NAK and redeliver; there is
// no position-readiness callback or DLQ. The legacy unidentified slot needs a
// complete corrected retained-state rebuild, not one unrelated valid update.
func TestPositionIdentityJetStreamNAKRepairRestartAndDecisions(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL real canonical JetStream")
	}
	for _, kind := range []string{"canonical-slot", "legacy-portfolio"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			id := "position-identity-" + uuid.NewString()
			tenant, portfolio := "tenant-"+id, "PF-"+id
			topics := []string{subject.PositionFor(tenant, portfolio, "MSFT"), subject.PositionFor(tenant, portfolio, "AAPL")}
			filter := strings.TrimSuffix(topics[0], ".MSFT") + ".>"
			admin, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(admin.Close)
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatal(err)
			}
			input := positionTimeStream(t, ctx, js, topics[0])
			info, err := input.Info(ctx)
			if err != nil || info.Config.Name != "POSITION" || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
				t.Fatalf("fixture requires canonical never-ageing POSITION compaction: %+v %v", info, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			// Registered before publication, scoped to two unique input subjects.
			// Subscription joins/deletion run first, while both connections are open.
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				for _, topic := range topics {
					if err := input.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
						t.Error(err)
					}
				}
			})
			in, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms/position-identity-test", ProducerVersion: "test", Tenant: tenant})
			if err != nil {
				t.Fatal(err)
			}
			out, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance/position-identity-test", ProducerVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			pub := &positionIdentityPublisher{producer: out}
			reg := comp.NewMandateRegistry()
			positionIdentityMandate(t, reg, tenant, portfolio)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			makeMonitor := func() *Monitor {
				m := NewMonitor(nil, reg, nil, NewEmitter(pub), compliancebus.NewBusRecorder(pub, logger), logger)
				m.now = clockAt(t0.Add(24 * time.Hour))
				return m
			}
			publish := func(slot, instrument string, qty, value int64, at time.Time, legacy bool) string {
				t.Helper()
				topic := subject.PositionFor(tenant, portfolio, slot)
				key := topic // actual OMS entity partition key, not a test-only identifier
				if legacy {
					key = portfolio
				}
				var payload domainpb.PositionState
				if err := proto.Unmarshal(positionTimePayload(t, portfolio, instrument, qty, value, timestamppb.New(at)), &payload); err != nil {
					t.Fatal(err)
				}
				if err := in.Publish(ctx, bus.Event{Subject: topic, EventType: subject.PositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
					SchemaVersion: 1, Domain: "risk", EventTime: at, PartitionKey: key, CorrelationID: id, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: &payload}); err != nil {
					t.Fatal(err)
				}
				retained, err := input.GetLastMsgForSubject(ctx, topic)
				if err != nil {
					t.Fatal(err)
				}
				env, _, err := bus.Unframe(retained.Data)
				if err != nil || bus.Validate(env) != nil {
					t.Fatal("input did not cross canonical Producer framing/validation")
				}
				return env.GetEventId()
			}
			type subscription struct {
				consumer jetstream.Consumer
				seen     <-chan positionIdentityDelivery
				stop     func()
			}
			start := func(m *Monitor) subscription {
				t.Helper()
				transport := &positionTimeTransport{NATSClient: client, created: make(chan [2]string, 1)}
				consumer, err := bus.NewConsumer(transport)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				seen, done := make(chan positionIdentityDelivery, 64), make(chan error, 1)
				go func() {
					done <- consumer.SubscribeBroadcast(subCtx, filter, func(c context.Context, env *envelopepb.Envelope, payload []byte) error {
						err := m.Handle(c, env, payload)
						select {
						case seen <- positionIdentityDelivery{proto.Clone(env).(*envelopepb.Envelope), append([]byte(nil), payload...), err}:
						case <-subCtx.Done():
						}
						return err
					})
				}()
				var handle [2]string
				var once sync.Once
				finish := func() {
					once.Do(func() {
						stop()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("position identity broadcast did not stop")
						}
						if handle[0] != "" {
							cleanup, end := context.WithTimeout(context.Background(), 5*time.Second)
							defer end()
							if err := js.DeleteConsumer(cleanup, handle[0], handle[1]); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
								t.Error(err)
							}
						}
					})
				}
				t.Cleanup(finish)
				select {
				case handle = <-transport.created:
				case <-ctx.Done():
					t.Fatal("production broadcast did not create its consumer")
				}
				owned, err := js.Consumer(ctx, handle[0], handle[1])
				if err != nil {
					t.Fatal(err)
				}
				return subscription{owned, seen, finish}
			}
			await := func(s subscription, eventID string, wantError bool) positionIdentityDelivery {
				t.Helper()
				for {
					select {
					case d := <-s.seen:
						if d.env.GetEventId() != eventID {
							continue
						}
						if (d.err != nil) != wantError {
							t.Fatalf("unexpected identity disposition: %v want error=%v", d.err, wantError)
						}
						return d
					case <-ctx.Done():
						t.Fatal("expected identity delivery never reached the production handler")
					}
				}
			}
			at := t0.Add(time.Hour + 123456789)
			publish("MSFT", "MSFT", 1, 100, at, false)
			publish("AAPL", "AAPL", 1, 100, at, false)
			live := makeMonitor()
			active := start(live)
			positionTimeAwaitACK(t, ctx, active.consumer, 2)
			// Start observers after the initial fold; they read the actual retained
			// decision and breach routes, without modifying global output history.
			outputs := []jetstream.Consumer{}
			for i, topic := range []string{compliancebus.SubjectDecision, comp.SubjectBreach} {
				stream := positionTimeStream(t, ctx, js, topic)
				name := id + "-" + []string{"decision", "breach"}[i]
				c, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: name, FilterSubject: topic, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, end := context.WithTimeout(context.Background(), 5*time.Second)
					defer end()
					if err := stream.DeleteConsumer(cleanup, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Error(err)
					}
				})
				outputs = append(outputs, c)
			}
			before := pub.calls.Load()
			poisonID := publish("MSFT", "", 9, 900, at.Add(time.Second), kind == "legacy-portfolio")
			first, repeated := await(active, poisonID, true), await(active, poisonID, true)
			if !proto.Equal(first.env, repeated.env) || !bytes.Equal(first.payload, repeated.payload) {
				t.Fatal("NAK redelivery changed original canonical bytes")
			}
			pending, err := active.consumer.Info(ctx)
			if err != nil || pending.NumAckPending == 0 {
				t.Fatalf("unidentified state was broker-acknowledged: %+v %v", pending, err)
			}
			if err := live.Reevaluate(ctx, tenant, portfolio); err == nil || pub.calls.Load() != before {
				t.Fatal("unknown live book emitted a measured result")
			}
			unrelated := publish("AAPL", "AAPL", 1, 150, at.Add(2*time.Second), false)
			await(active, unrelated, true)
			repair := publish("MSFT", "MSFT", 1, 100, at.Add(3*time.Second), false)
			await(active, repair, kind == "legacy-portfolio")
			// Keep the non-capped holding first in retained replay order. The
			// production monitor evaluates as each slot arrives; this fixture is
			// about identity recovery, not a new initial-book evaluation barrier.
			reaffirm := publish("AAPL", "AAPL", 1, 150, at.Add(3*time.Second+1), false)
			await(active, reaffirm, kind == "legacy-portfolio")
			if kind == "canonical-slot" {
				positionTimeAwaitACK(t, ctx, active.consumer, 4)
				if err := live.Reevaluate(ctx, tenant, portfolio); err != nil {
					t.Fatal(err)
				}
			} else if live.Reevaluate(ctx, tenant, portfolio) == nil {
				t.Fatal("one identified repair cleared the legacy unidentified slot")
			}
			active.stop()
			restarted := makeMonitor()
			fresh := start(restarted)
			positionTimeAwaitACK(t, ctx, fresh.consumer, 2)
			if err := restarted.Reevaluate(ctx, tenant, portfolio); err != nil || pub.calls.Load() != before {
				t.Fatal("corrected retained replay did not recover the exact 60% boundary", err)
			}
			breachAt := at.Add(4 * time.Second)
			breachID := publish("AAPL", "AAPL", 4, 400, breachAt, false)
			await(fresh, breachID, false)
			positionTimeAwaitACK(t, ctx, fresh.consumer, 3)
			for i, c := range outputs {
				msg, err := c.Next(jetstream.FetchMaxWait(5 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				env, data, err := bus.Unframe(msg.Data())
				if err != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetCausationId() != breachID || env.GetCorrelationId() != id || !env.GetEventTime().AsTime().Equal(breachAt) {
					t.Fatal("measured output lost canonical envelope, tenant, time or lineage")
				}
				if i == 0 {
					var decision observationpb.DecisionLog
					if proto.Unmarshal(data, &decision) != nil || env.GetEventType() != compliancebus.DecisionEventType || decision.Attributes["portfolio_id"] != portfolio || decision.Attributes["status"] != "BREACH" {
						t.Fatal("actual decision recorder did not record the recovered breach")
					}
				} else {
					var breach compliancepb.ComplianceBreach
					if proto.Unmarshal(data, &breach) != nil || env.GetEventType() != comp.EventTypeBreach || breach.GetPortfolioId() != portfolio || breach.GetMandateVersion() != 7 || breach.GetTrigger() != compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE || !breach.GetDetectedAt().AsTime().Equal(breachAt) || breach.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
						t.Fatal("actual breach FACT lost its mandate, trigger or observation")
					}
				}
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
			}
			for _, instrument := range []string{"AAPL", "MSFT"} {
				id := publish(instrument, instrument, 0, 0, at.Add(5*time.Second), false)
				await(fresh, id, false)
			}
			positionTimeAwaitACK(t, ctx, fresh.consumer, 5)
			if pub.calls.Load() != before+2 {
				t.Fatal("poison, repair or true-flat eviction emitted an extra measured outcome")
			}
			for _, c := range outputs {
				info, err := c.Info(ctx)
				if err != nil || info.NumPending != 0 || info.NumAckPending != 0 {
					t.Fatalf("unexpected or unacknowledged output: %+v %v", info, err)
				}
			}
		})
	}
}
