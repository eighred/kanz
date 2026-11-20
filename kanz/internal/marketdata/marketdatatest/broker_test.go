package marketdatatest

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type capturedFrames struct {
	bus.Client
	messages []bus.Message
}

func (c *capturedFrames) Publish(_ context.Context, message bus.Message) error {
	c.messages = append(c.messages, message)
	return nil
}

// Check the actual framed bytes before transport. Broker ACKs and durable
// replay are exercised by the two owners using the JetStream fixture.
func TestPublishFramesUsePayloadInstrumentAndKnowledgeTime(t *testing.T) {
	client := &capturedFrames{}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "payload-time-test", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	b := Broker{subject: "market.time.contract", producer: producer}
	first := time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)
	for i, id := range []string{"series-A", "series-B"} {
		at := first.Add(time.Duration(i) * time.Minute)
		input := &marketpb.MarketDataEvent{InstrumentId: id}
		b.Publish(t, t.Context(), []Frame{{EventType: "market.crypto.trade", Payload: input}}, at)
		if len(client.messages) != i+1 {
			t.Fatalf("publication %d produced %d total frames", i, len(client.messages))
		}
		msg := client.messages[i]
		env, data, err := bus.Unframe(msg.Body)
		if err != nil {
			t.Fatal(err)
		}
		var decoded marketpb.MarketDataEvent
		if err := proto.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if msg.Subject != "market.time.contract.0" || !proto.Equal(input, &decoded) ||
			env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != "market.crypto.trade" ||
			env.GetPartitionKey() != decoded.GetInstrumentId() || env.GetTenantId() != "__system__" ||
			!env.GetEventTime().AsTime().Equal(at) || !env.GetIngestionTime().AsTime().Equal(at) {
			t.Fatalf("frame %d lost its route, payload identity or dated observation", i)
		}
	}
}
