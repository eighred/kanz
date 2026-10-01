package auditdelivery

import (
	"context"
	"errors"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type wireCapture struct{ message bus.Message }

func (c *wireCapture) Publish(_ context.Context, m bus.Message) error { c.message = m; return nil }
func (c *wireCapture) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("unused")
}
func (c *wireCapture) Close() error { return nil }

func TestAuthorityEnvelopeRetainsSourceIdentityTimeAndTenant(t *testing.T) {
	wire := &wireCapture{}
	producer, err := bus.NewProducer(wire, bus.ProducerConfig{Source: "authority", ProducerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-365 * 24 * time.Hour)
	d := &observationpb.DecisionLog{Decider: "identity", Attributes: map[string]string{"principal.subject": "operator", "principal.tenant": "acme", "action": "identity.account.access"}}
	if err = producer.Publish(t.Context(), eventFor(id.String(), "acme", at, d)); err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(wire.message.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err = bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	var actual observationpb.DecisionLog
	if err = proto.Unmarshal(payload, &actual); err != nil {
		t.Fatal(err)
	}
	if env.EventType != "platform.authz.decision" || env.Domain != "platform" || env.EventId != id.String() || env.TenantId != "acme" || !env.EventTime.AsTime().Equal(at) || wire.message.Subject != Subject || !proto.Equal(d, &actual) {
		t.Fatalf("source evidence changed: %+v %+v", env, &actual)
	}
}
