package bus_test

import (
	"context"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
)

func TestPersistedEventIdentitySurvivesProducerRestart(t *testing.T) {
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	e := factEvent()
	e.EventID = id.String()
	for i := 0; i < 2; i++ {
		p, c := newTestProducer(t)
		if err = p.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		env, _, err := bus.Unframe(c.sent[0].Body)
		if err != nil || env.EventId != e.EventID || env.IdempotencyKey != e.EventID || c.sent[0].Headers["Nats-Msg-Id"] != e.EventID {
			t.Fatalf("persisted identity changed: %v %+v", err, env)
		}
	}
}

func TestPersistedEventIdentityRejectsInvalidAndCommandOverrides(t *testing.T) {
	for _, id := range []string{"not-a-uuid", uuid.NewString(), "00000000-0000-7000-0000-000000000000"} {
		p, c := newTestProducer(t)
		e := factEvent()
		e.EventID = id
		if err := p.Publish(context.Background(), e); err == nil || len(c.sent) != 0 {
			t.Fatalf("invalid persisted identity published: %q", id)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	p, c := newTestProducer(t)
	e := factEvent()
	e.EventID = id.String()
	e.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	e.IdempotencyKey = "command-key"
	if err = p.Publish(context.Background(), e); err == nil || len(c.sent) != 0 {
		t.Fatal("command event identity override accepted")
	}
}
