package bus_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Only the existing privileged POSITION harness grants this separate broker.
// It has no OMS and cannot produce effects on the primary reused-broker fixture.
// Ordinary lanes do not claim this integration from their supplied main broker.
func TestConsumerDispatchScopeOnOwnedBroker(t *testing.T) {
	flag := os.Getenv("TEST_POSITION_DEDUP_CONTROL")
	if flag == "" {
		t.Skip("requires the explicit owned POSITION dedup-control fixture")
	}
	if flag != "1" || os.Getenv("TEST_NATS_URL") != "nats://localhost:4225" {
		t.Fatal("owned dedup control requires its exact granted local broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	admin, err := nats.Connect("nats://localhost:4225")
	if err != nil {
		t.Fatal("owned dedup broker connection refused")
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("owned dedup JetStream connection refused")
	}
	stream, err := js.Stream(ctx, "EXECUTION")
	if err != nil {
		t.Fatal("canonical EXECUTION stream missing on owned broker")
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: "nats://localhost:4225", Name: "owned-dispatch-control"})
	if err != nil {
		t.Fatal("owned dedup client refused")
	}
	t.Cleanup(func() { _ = client.Close() })
	nonce := fmt.Sprintf("dispatch-%d", time.Now().UnixNano())
	groups := [2]string{nonce + "-positions", nonce + "-costs"}
	orders := [2]string{nonce + "-input", nonce + "-marker"}
	const filled = "order.order.filled"
	// Register the exact possible indirect output keys without asserting an OMS
	// runs here. No primary broker, stream configuration or broad purge is used.
	bustest.ReclaimSubjects(t, js, subject.PositionFor(bus.SystemTenant, nonce, "BTC-USD"), subject.VenuePositionFor(bus.SystemTenant, nonce, "XNAS", "BTC-USD"))
	transport := &ownedDispatchTransport{NATSClient: client, groups: groups, orders: orders, progress: make(chan struct{}, 1)}
	consumer, err := bus.NewConsumer(transport)
	if err != nil {
		t.Fatal("owned dedup consumer refused")
	}
	subctx, stop := context.WithCancel(ctx)
	done := [2]chan error{make(chan error, 1), make(chan error, 1)}
	t.Cleanup(func() {
		stop()
		cleanup, finish := context.WithTimeout(context.Background(), 8*time.Second)
		defer finish()
		for _, result := range done {
			select {
			case err := <-result:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Error("owned durable subscription did not stop cleanly")
				}
			case <-cleanup.Done():
				t.Error("owned durable subscription did not stop/join")
				return // never delete a consumer while its handler may still run
			}
		}
		for _, group := range groups {
			// This exact durable spelling is the NATSClient group/subject contract.
			name := group + "-order_order_filled"
			owned, err := stream.Consumer(cleanup, name)
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				continue
			}
			if err != nil {
				t.Error("owned durable cleanup could not establish its identity")
				continue
			}
			info, err := owned.Info(cleanup)
			if err != nil || info.Name != name || info.Config.Durable != name || info.Config.FilterSubject != filled {
				t.Error("owned durable cleanup refused a changed identity")
				continue
			}
			if err := js.DeleteConsumer(cleanup, "EXECUTION", name); err != nil {
				t.Error("owned durable cleanup failed")
				continue
			}
			if _, err := stream.Consumer(cleanup, name); !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Error("owned durable absence was not observed")
			}
		}
	})
	armed := make(chan struct{}, 2)
	for index, group := range groups {
		go func() {
			done[index] <- consumer.SubscribeReady(subctx, filled, group, func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
				return transport.effect(index, payload)
			}, func() { armed <- struct{}{} })
		}()
	}
	for range groups {
		select {
		case <-armed:
		case <-ctx.Done():
			t.Fatal("independent durable group did not arm")
		}
	}
	producer, err := bus.NewProducer(transport, bus.ProducerConfig{Source: "fixture/dispatch-scope", ProducerVersion: "test", Tenant: bus.SystemTenant})
	if err != nil {
		t.Fatal("owned canonical producer refused")
	}
	publish := func(id string) {
		t.Helper()
		now := time.Now().UTC()
		fill := &orderpb.Fill{OrderId: id, FillId: id + "-fill", InstrumentId: "BTC-USD", Venue: "XNAS", VenueAccountId: "fixture-account", VenueExecutionId: id + "-execution", Side: orderpb.Side_SIDE_BUY,
			Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, ExecutedAt: timestamppb.New(now)}
		fact := &orderpb.OrderFilled{OrderId: id, Fill: fill, State: &orderpb.OrderState{OrderId: id, PortfolioId: nonce, InstrumentId: "BTC-USD", Venue: "XNAS", Side: orderpb.Side_SIDE_BUY, FilledQuantity: fill.Quantity}}
		if err := producer.Publish(ctx, bus.Event{Subject: filled, EventType: filled, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", EventTime: now, PartitionKey: id, Payload: fact}); err != nil {
			t.Fatal("owned canonical fill publish refused")
		}
	}
	await := func(wantDuplicate, wantMarker bool) {
		t.Helper()
		for {
			deliveries, effects := transport.counts()
			complete := true
			for index := range groups {
				complete = complete && effects[index][0] == 1
				if wantDuplicate {
					complete = complete && deliveries[index][0] >= 2
				}
				if wantMarker {
					complete = complete && effects[index][1] == 1
				}
			}
			if complete {
				return
			}
			select {
			case <-transport.progress:
			case <-ctx.Done():
				if wantDuplicate || wantMarker {
					t.Fatal("owned duplicate or marker completion missing")
				}
				for _, counts := range [][2][2]int{deliveries, effects} {
					for _, group := range counts {
						for _, count := range group {
							if count < 0 || count > 128 {
								t.Fatal("owned dispatch refusal witness exceeded its bound")
							}
						}
					}
				}
				t.Logf("owned dispatch scope refusal witness: deliveries=%d,%d effects=%d,%d marker_deliveries=%d,%d marker_effects=%d,%d",
					deliveries[0][0], deliveries[1][0], effects[0][0], effects[1][0], deliveries[0][1], deliveries[1][1], effects[0][1], effects[1][1])
				t.Fatal("independent durable group dispatch missing")
			}
		}
	}
	publish(orders[0])
	await(false, false)
	first := transport.firstFrame()
	// Keep the exact validated envelope/payload bytes, but omit Nats-Msg-Id:
	// the broker must deliver the duplicate so this tests Consumer suppression.
	if err := client.Publish(ctx, bus.Message{Subject: filled, Body: first}); err != nil {
		t.Fatal("owned duplicate frame publish refused")
	}
	await(true, false)
	publish(orders[1])
	await(true, true)
	deliveries, effects := transport.counts()
	if deliveries != [2][2]int{{2, 1}, {2, 1}} || effects != [2][2]int{{1, 1}, {1, 1}} {
		t.Fatal("owned duplicate delivery or per-group effect counts differ")
	}
	t.Logf("owned dispatch scope receipts: groups=2 deliveries=3,3 effects=2,2 repeated_frame_sha256=%x", sha256.Sum256(first))
}

type ownedDispatchTransport struct {
	*bus.NATSClient
	mu                  sync.Mutex
	groups, orders      [2]string
	deliveries, effects [2][2]int
	first               []byte
	progress            chan struct{}
}

func (s *ownedDispatchTransport) Publish(ctx context.Context, message bus.Message) error {
	if len(message.Body) == 0 || len(message.Body) > 16384 {
		return errors.New("owned canonical frame exceeded its bound")
	}
	s.mu.Lock()
	if s.first == nil {
		s.first = append([]byte(nil), message.Body...)
	}
	s.mu.Unlock()
	return s.NATSClient.Publish(ctx, message)
}

func (s *ownedDispatchTransport) firstFrame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.first...)
}

func (s *ownedDispatchTransport) signal() {
	select {
	case s.progress <- struct{}{}:
	default:
	}
}

func (s *ownedDispatchTransport) orderIndex(payload []byte) (int, error) {
	var fact orderpb.OrderFilled
	if proto.Unmarshal(payload, &fact) == nil {
		for index, id := range s.orders {
			if fact.OrderId == id && fact.GetFill().GetOrderId() == id {
				return index, nil
			}
		}
	}
	return 0, errors.New("unexpected owned fill identity")
}

func (s *ownedDispatchTransport) effect(group int, payload []byte) error {
	index, err := s.orderIndex(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.effects[group][index]++
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *ownedDispatchTransport) SubscribeReady(ctx context.Context, subject, group string, handler bus.Handler, ready func()) error {
	index := -1
	for candidate, name := range s.groups {
		if group == name {
			index = candidate
		}
	}
	if index < 0 {
		return errors.New("unexpected owned durable group")
	}
	return s.NATSClient.SubscribeReady(ctx, subject, group, func(ctx context.Context, message bus.Message) error {
		env, payload, err := bus.Unframe(message.Body)
		if err != nil || bus.Validate(env) != nil {
			return errors.New("invalid owned canonical delivery")
		}
		order, err := s.orderIndex(payload)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.deliveries[index][order]++
		s.mu.Unlock()
		s.signal()
		return handler(ctx, message)
	}, ready)
}

func (s *ownedDispatchTransport) counts() ([2][2]int, [2][2]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries, s.effects
}
