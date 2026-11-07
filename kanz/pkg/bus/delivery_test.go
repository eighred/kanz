package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"testing"
)

type deliveryPublisher struct{ messages []bus.Message }

func (p *deliveryPublisher) Publish(_ context.Context, m bus.Message) error {
	p.messages = append(p.messages, m)
	return nil
}

func TestDeliveryOriginalBytesRetriesAndDLQ(t *testing.T) {
	original := append(frame(t, validEnvelope(), []byte("original")), 0x0a, 0x00, 0xa0, 0x06, 0x81, 0x00)
	var decoded envelopepb.EventFrame
	if err := proto.Unmarshal(original, &decoded); err != nil {
		t.Fatal(err)
	}
	normalized, err := proto.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, normalized) {
		t.Fatal("fixture does not distinguish original bytes from remarshaling")
	}
	sub := &oneShotSub{msg: bus.Message{Body: original, Subject: "x", Kafka: &bus.KafkaDelivery{Topic: "real-topic", Partition: 0, Offset: 0}}}
	parked := &deliveryPublisher{}
	c, err := bus.NewConsumer(sub, fastRetry(2), bus.WithDLQ(parked))
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err = c.SubscribeDelivery(context.Background(), "x", "g", func(_ context.Context, _ *envelopepb.Envelope, _ []byte, d bus.Delivery) error {
		attempts++
		if !bytes.Equal(original, d.Frame) || d.Kafka == nil || d.Kafka.Topic != "real-topic" || d.Kafka.Partition != 0 || d.Kafka.Offset != 0 {
			t.Fatal("delivery changed across attempts")
		}
		d.Frame[0] ^= 0xff
		d.Kafka.Topic = "mutated"
		d.Kafka.Offset = 99
		return errors.New("landing unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(parked.messages) != 1 || !bytes.Equal(parked.messages[0].Body, original) || sub.msg.Kafka.Topic != "real-topic" {
		t.Fatal("retry/parking contract changed")
	}
}

func TestDeliveryDedupAndUnknownProvenance(t *testing.T) {
	sub := &multiShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}, times: 2}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = c.SubscribeDelivery(context.Background(), "x", "g", func(_ context.Context, _ *envelopepb.Envelope, _ []byte, d bus.Delivery) error {
		calls++
		if d.Kafka != nil {
			t.Fatal("fabricated provenance")
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestDeliveryValidationPrecedesDispatch(t *testing.T) {
	for _, body := range [][]byte{{0xff}, frame(t, &envelopepb.Envelope{}, nil)} {
		parked := &deliveryPublisher{}
		c, err := bus.NewConsumer(&oneShotSub{msg: bus.Message{Subject: "x", Body: body}}, bus.WithDLQ(parked))
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		err = c.SubscribeDelivery(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte, bus.Delivery) error { calls++; return nil })
		if err != nil || calls != 0 || len(parked.messages) != 1 || !bytes.Equal(parked.messages[0].Body, body) {
			t.Fatal("validation/parking dispatch changed")
		}
	}
}

func TestDeliveryBoundDoesNotChangeLegacySubscriber(t *testing.T) {
	original := frame(t, validEnvelope(), make([]byte, bus.MaxDeliveryFrameBytes))
	sub := &oneShotSub{msg: bus.Message{Subject: "x", Body: original}}
	raw, err := bus.NewConsumer(sub, fastRetry(1))
	if err != nil {
		t.Fatal(err)
	}
	dispatched := false
	err = raw.SubscribeDelivery(context.Background(), "x", "raw", func(context.Context, *envelopepb.Envelope, []byte, bus.Delivery) error { dispatched = true; return nil })
	if err == nil || dispatched {
		t.Fatal("oversized raw custody dispatched")
	}
	legacy, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatal(err)
	}
	err = legacy.Subscribe(context.Background(), "x", "legacy", func(context.Context, *envelopepb.Envelope, []byte) error { dispatched = true; return nil })
	if err != nil || !dispatched {
		t.Fatal("raw-only bound changed legacy dispatch")
	}
}

func TestReceiveMetadataDoesNotChangeMessageJSON(t *testing.T) {
	message := bus.Message{Subject: "topic", Key: []byte("key"), Body: []byte("body"), Headers: map[string]string{"h": "v"}}
	before, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	message.Kafka = &bus.KafkaDelivery{Topic: "topic", Partition: 0, Offset: 0}
	after, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte("Kafka")) {
		t.Fatal("receive metadata changed original JSON shape")
	}
}
