//go:build synthetic_handoff

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
)

// The outbox publisher is reached only after the terminal SQL call commits
// and before the relay writes its publication mark.
type stopAtSettlementFact struct {
	order.Bus
	eventID, orderID, subject string
	afterPublish              bool
}

func (p stopAtSettlementFact) Publish(ctx context.Context, event bus.Event) error {
	if event.Subject == p.subject && event.PartitionKey == p.orderID && event.CausationID == p.eventID {
		if p.afterPublish {
			if err := p.Bus.Publish(ctx, event); err != nil {
				return err
			}
			fmt.Println("GO_HANDBACK stopped after JetStream accepted the FACT and before the SQL publication mark")
			os.Exit(73)
		}
		fmt.Println("GO_HANDBACK stopped after terminal commit and before FACT publication")
		os.Exit(72)
	}
	return p.Bus.Publish(ctx, event)
}

func restartSyntheticFundedWithdrawal(t *testing.T, ctx context.Context, client *bus.NATSClient, svc *order.Service, broker, eventID, stage, raceDir string) {
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
	group, budget := "synthetic-go-funded-withdrawal", 12*time.Second
	if raceDir != "" {
		group, budget = group+"-"+stage, 20*time.Second
	}
	ctx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithRetainedRetries(subject))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 3)
	done := make(chan error, 1)
	started := false
	go func() {
		done <- consumer.Subscribe(ctx, subject, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.GetEventId() != eventID {
				err := bus.Terminal(errors.New("unexpected event in successor withdrawal durable"))
				result <- err
				return err
			}
			if raceDir != "" && !started {
				started = true
				if err := awaitWithdrawalRace(ctx, raceDir, stage); err != nil {
					result <- err
					return err
				}
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
	if raceDir != "" {
		awaitRacingWithdrawal(t, ctx, commands, subject, group, result)
		return
	}
	select {
	case err := <-result:
		if (stage != "publish" && stage != "publish-mark") || err != nil {
			t.Fatalf("withdrawal stage %s returned unexpectedly: %v", stage, err)
		}
	case <-ctx.Done():
		t.Fatalf("missing broker delivery for withdrawal stage %s", stage)
	}
	awaitSyntheticCancelAck(t, ctx, commands, subject, group, 3)
	t.Log("GO_HANDBACK third process published the retained FACT and acknowledged delivery three on the original durable")
}

func awaitWithdrawalRace(ctx context.Context, dir, stage string) error {
	ready := filepath.Join(dir, "ready-"+strings.TrimPrefix(stage, "race-"))
	if err := os.WriteFile(ready, nil, 0600); err != nil {
		return err
	}
	release := filepath.Join(dir, "release")
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		_, err := os.Stat(release)
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

func awaitRacingWithdrawal(t *testing.T, ctx context.Context, commands jetstream.Stream, subject, group string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if status.Code(err) != codes.DeadlineExceeded && (err == nil || err.Error() != "oms: synthetic withdrawal has no matching terminal execution history") {
			t.Fatalf("first racing delivery neither lost the venue ACK nor observed an unresolved withdrawal: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("first racing delivery did not reach OMS")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("second racing delivery did not reconcile: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("racing durable did not redeliver")
	}
	awaitSyntheticCancelAck(t, ctx, commands, subject, group, 2)
	t.Logf("GO_HANDBACK %s acknowledged the original withdrawal on delivery two", group)
}
