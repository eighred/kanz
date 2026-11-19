package main

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelope "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

type wireCapture struct {
	message bus.Message
}

func (c *wireCapture) Publish(_ context.Context, m bus.Message) error {
	c.message = m
	return nil
}

func (*wireCapture) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("unexpected subscription")
}

func (*wireCapture) Close() error { return nil }

func TestRehearsalPublishesCanonicalEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		op, source, route, eventType, schema, key string
		message                                   proto.Message
	}{
		{"snapshot", "feed", "accounting.collateral.snapshot", "accounting.collateral.snapshot", "collateral.v1.WorkflowSnapshot:1", portfolio, &pb.WorkflowSnapshot{}},
		{"confirm", "custody", "accounting.collateral.confirmation.custody", "accounting.collateral.confirmation", "collateral.v1.PostingConfirmation:1", workflow, &pb.PostingConfirmation{}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			cfg, event := fixturePublication(tc.op, time.Now().UTC())
			wire := &wireCapture{}
			producer, err := bus.NewProducer(wire, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := producer.Publish(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			env, data, err := bus.Unframe(wire.message.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := bus.Validate(env); err != nil {
				t.Fatal(err)
			}
			if env.Source != tc.source || env.TenantId != tenant || env.PartitionKey != tc.key || wire.message.Subject != tc.route ||
				env.EventClass != envelope.EventClass_EVENT_CLASS_FACT || env.Domain != "accounting" || env.EventType != tc.eventType ||
				env.PayloadSchemaRef != tc.schema || env.SchemaVersion != 1 {
				t.Fatal("published fixture identity or route differs")
			}
			if err := proto.Unmarshal(data, tc.message); err != nil || !proto.Equal(event.Payload, tc.message) {
				t.Fatal("published fixture payload differs")
			}
		})
	}
}

func TestGatewayChecksTheWholeWorkflowDecimalDomain(t *testing.T) {
	const valid = `{"legs":[{"quantity":{"coefficient":"60"},"marketValue":{"coefficient":"1","exponent":64}}]}`
	const invalid = `{"legs":[{"quantity":{"coefficient":"60"},"marketValue":{"coefficient":"1","exponent":65}}]}`
	if err := decodeGatewayState([]byte(valid), new(pb.WorkflowRecorded)); err != nil {
		t.Fatal(err)
	}
	if err := decodeGatewayState([]byte(invalid), new(pb.WorkflowRecorded)); err == nil {
		t.Fatal("accepted an out-of-domain market value with a valid quantity")
	}
}
