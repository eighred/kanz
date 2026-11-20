//go:build synthetic_handoff

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func replaySyntheticFundedCancel(t *testing.T, ctx context.Context, client *bus.NATSClient, svc *order.Service, broker, eventID string) {
	t.Helper()
	nc, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := js.Stream(ctx, "SYNTHETIC_ORDERS")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	before, err := facts.Info(ctx)
	if err != nil || before.State.Msgs != 4 {
		t.Fatalf("retained broker facts before Go replay: %v", err)
	}
	subject := bus.TenantRoutedSubject(bus.TenantIDFromContext(ctx), order.SubjectCancel)
	const group = "synthetic-go-retained-cancel"
	durable := group + "-" + strings.ReplaceAll(subject, ".", "_")
	prior, err := commands.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: durable, AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := prior.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(msg.Data())
	if err != nil || env.GetEventId() != eventID {
		t.Fatalf("handoff cancellation did not match the broker record: %v", err)
	}
	if err := msg.Nak(); err != nil {
		t.Fatal(err)
	}
	// A new Go consumer must actually dispatch delivery two and ACK it. A
	// second consumer starts with empty dedup state and dispatches it again.
	deliverSyntheticFundedCancel(t, ctx, client, svc, commands, subject, group, eventID, 2)
	deliverSyntheticFundedCancel(t, ctx, client, svc, commands, subject, group+"-restart", eventID, 1)

	other := proto.CloneOf(env)
	other.EventId += "-different"
	if err := svc.Handle(ctx, other, payload); !bus.IsTerminal(err) {
		t.Fatalf("unmatched cancellation must fail closed, got %v", err)
	}
	after, err := facts.Info(ctx)
	if err != nil || after.State.LastSeq != before.State.LastSeq || after.State.Msgs != before.State.Msgs {
		t.Fatalf("Go replay changed the broker fact sequence: %v", err)
	}
	if _, err := facts.GetLastMsgForSubject(ctx, order.EventTypeOutcome); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Fatalf("Go replay emitted another command outcome: %v", err)
	}
	t.Log("GO_HANDBACK The ordinary OMS consumer twice handled and acknowledged the retained gateway cancellation without emitting another fact")
}

func deliverSyntheticFundedCancel(t *testing.T, ctx context.Context, client *bus.NATSClient, svc *order.Service,
	commands jetstream.Stream, subject, group, eventID string, deliveries uint64,
) {
	t.Helper()
	ctx, stop := context.WithTimeout(ctx, 5*time.Second)
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		stop()
		t.Fatal(err)
	}
	result := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, subject, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.GetEventId() != eventID {
				return errors.New("unexpected event in successor cancellation durable")
			}
			err := svc.Handle(ctx, env, payload)
			result <- err
			return err
		})
	}()
	defer func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("ordinary cancellation subscription: %v", err)
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ordinary cancellation handler refused the retained command: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("ordinary cancellation handler did not receive the replay")
	}
	awaitSyntheticCancelAck(t, ctx, commands, subject, group, deliveries)
}

func awaitSyntheticCancelAck(t *testing.T, ctx context.Context, commands jetstream.Stream, subject, group string, deliveries uint64) {
	t.Helper()
	durable := group + "-" + strings.ReplaceAll(subject, ".", "_")
	for ctx.Err() == nil {
		info, err := commands.Consumer(ctx, durable)
		if err == nil {
			state, err := info.Info(ctx)
			if err == nil && state.Delivered.Consumer == deliveries && state.AckFloor.Consumer == deliveries && state.NumAckPending == 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("broker did not acknowledge cancellation at delivery %d", deliveries)
}

func consumeSyntheticFundedWithdrawal(t *testing.T, ctx context.Context, client *bus.NATSClient, svc *order.Service, broker, eventID string, contradiction bool) {
	t.Helper()
	nc, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := js.Stream(ctx, "SYNTHETIC_ORDERS")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	before, err := facts.Info(ctx)
	if err != nil || before.State.Msgs != 3 {
		t.Fatalf("new withdrawal requires exactly three earlier OMS facts: %v", err)
	}
	subject := bus.TenantRoutedSubject(bus.TenantIDFromContext(ctx), order.SubjectCancel)
	const group = "synthetic-go-funded-withdrawal"
	ctx, stop := context.WithTimeout(ctx, 12*time.Second)
	defer stop()
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithRetainedRetries(subject))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 3)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, subject, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.GetEventId() != eventID {
				return bus.Terminal(errors.New("unexpected event in successor withdrawal durable"))
			}
			err := svc.Handle(ctx, env, payload)
			result <- err
			return err
		})
	}()
	defer func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("withdrawal subscription: %v", err)
		}
	}()
	select {
	case err := <-result:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("first withdrawal must lose its XSIM acknowledgement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("missing first withdrawal attempt")
	}
	deliveries := uint64(2)
	if contradiction {
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "synthetic withdrawal has no matching terminal execution history") {
				t.Fatalf("OMS did not reject the terminal fill history: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("missing contradictory terminal response")
		}
		t.Log("GO_HANDBACK OMS refused the terminal fill history on delivery two")
		deliveries++
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("redelivered withdrawal did not reconcile: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("missing broker redelivery after withdrawal acknowledgement loss")
	}
	awaitSyntheticCancelAck(t, ctx, commands, subject, group, deliveries)
	deliverSyntheticFundedCancel(t, ctx, client, svc, commands, subject, group+"-replay", eventID, 1)
	after, err := facts.Info(ctx)
	if err != nil || after.State.Msgs != before.State.Msgs+1 || after.State.LastSeq != before.State.LastSeq+1 {
		t.Fatalf("withdrawal and replay did not yield exactly one terminal FACT: %v", err)
	}
	t.Logf("GO_HANDBACK Go lost the XSIM cancel ACK, completed on JetStream delivery %d, and ACKed the completed command on a fresh durable", deliveries)
}
