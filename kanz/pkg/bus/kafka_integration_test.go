package bus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/eighred/kanz/pkg/bus"
)

func TestKafkaPublishSubscribe(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "test.bus." + suffix
	group := "test-consumer-" + suffix

	// Out-of-band topic provisioning. In production topics are created by
	// kanz/infra/kafka/topics-job.yaml; auto-create is disabled.
	setupConn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("setup dial: %v", err)
	}
	if err := setupConn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create topic: %v", err)
	}
	setupConn.Close()
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(topic)
	})

	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "bus-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	received := make(chan bus.Message, 1)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := client.Subscribe(subCtx, topic, group, func(_ context.Context, m bus.Message) error {
			received <- m
			return nil
		}); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("subscribe ended: %v", err)
		}
	}()

	time.Sleep(2 * time.Second) // let the consumer join the group

	if err := client.Publish(context.Background(), bus.Message{
		Subject: topic,
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
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}
