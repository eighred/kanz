//go:build synthetic_handoff

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The outbox publisher is reached only after the terminal SQL call commits.
// Stop before sending to JetStream or returning the command handler's ACK.
type stopBeforeWithdrawalFact struct {
	order.Bus
	eventID, orderID string
}

func (p stopBeforeWithdrawalFact) Publish(ctx context.Context, event bus.Event) error {
	if event.Subject == order.EventTypeCancelled && event.PartitionKey == p.orderID && event.CausationID == p.eventID {
		fmt.Println("GO_HANDBACK stopped after terminal commit and before FACT publication")
		os.Exit(72)
	}
	return p.Bus.Publish(ctx, event)
}

func restartSyntheticFundedWithdrawal(t *testing.T, ctx context.Context, client *bus.NATSClient, svc *order.Service, broker, eventID, stage string) {
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
	subject := bus.TenantRoutedSubject(bus.TenantIDFromContext(ctx), order.SubjectCancel)
	const group = "synthetic-go-funded-withdrawal"
	ctx, stop := context.WithTimeout(ctx, 12*time.Second)
	defer stop()
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithRetainedRetries(subject))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, subject, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.GetEventId() != eventID {
				err := bus.Terminal(errors.New("unexpected event in successor withdrawal durable"))
				result <- err
				return err
			}
			err := svc.Handle(ctx, env, payload)
			if stage == "send" && status.Code(err) == codes.DeadlineExceeded {
				fmt.Println("GO_HANDBACK stopped after XSIM lost the withdrawal acknowledgement")
				os.Exit(71)
			}
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
		if stage != "publish" || err != nil {
			t.Fatalf("withdrawal stage %s returned unexpectedly: %v", stage, err)
		}
	case <-ctx.Done():
		t.Fatalf("missing broker delivery for withdrawal stage %s", stage)
	}
	awaitSyntheticCancelAck(t, ctx, commands, subject, group, 3)
	t.Log("GO_HANDBACK third process published the retained FACT and acknowledged delivery three on the original durable")
}
