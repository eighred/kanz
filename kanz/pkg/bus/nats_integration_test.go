package bus_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestNATSPublishSubscribe(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_BUS_" + suffix
	subject := "test.bus." + suffix

	// Out-of-band stream provisioning. The bus client itself doesn't manage
	// streams — in production they're provisioned via kanz/infra/nats/.
	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = setupJS.DeleteStream(ctx, streamName)
		setupConn.Close()
	})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	received := make(chan bus.Message, 1)
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = client.Subscribe(subCtx, subject, "test-consumer-"+suffix, func(_ context.Context, m bus.Message) error {
			received <- m
			return nil
		})
	}()

	time.Sleep(500 * time.Millisecond) // let the consumer bind

	if err := client.Publish(ctx, bus.Message{
		Subject: subject,
		Key:     []byte("partition-1"),
		Body:    []byte("hello"),
		Headers: map[string]string{"X-Test": "true"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case m := <-received:
		if string(m.Body) != "hello" {
			t.Errorf("body=%q want hello", m.Body)
		}
		if string(m.Key) != "partition-1" {
			t.Errorf("key=%q want partition-1", m.Key)
		}
		if m.Headers["X-Test"] != "true" {
			t.Errorf("X-Test=%q want true", m.Headers["X-Test"])
		}
		// natsKeyHeader must not leak into Headers — it's surfaced as Key.
		if _, leaked := m.Headers["Kanz-Partition-Key"]; leaked {
			t.Error("Kanz-Partition-Key leaked into Headers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}
