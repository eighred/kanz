package main

import (
	"context"
	"errors"
	"testing"
	"time"

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
	for _, tc := range []struct{ op, source, route, key string }{
		{"snapshot", "feed", "accounting.collateral.snapshot", portfolio},
		{"confirm", "custody", "accounting.collateral.confirmation.custody", workflow},
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
			if env.Source != tc.source || env.TenantId != tenant || env.PartitionKey != tc.key || wire.message.Subject != tc.route {
				t.Fatal("published fixture identity or route differs")
			}
			decoded := event.Payload.ProtoReflect().New().Interface()
			if err := proto.Unmarshal(data, decoded); err != nil || !proto.Equal(event.Payload, decoded) {
				t.Fatal("published fixture payload differs")
			}
		})
	}
}
