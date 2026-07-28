package bus_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// TestProducerTenantPrecedence pins the MT-01b stamping order:
// Event.TenantID > ctx tenant > ProducerConfig.Tenant.
func TestProducerTenantPrecedence(t *testing.T) {
	et := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	mk := func() bus.Event {
		return bus.Event{
			Subject: "market.equity.trade", EventType: "market.equity.trade",
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
			EventTime: et, PartitionKey: "AAPL", PayloadSchemaRef: "x:1",
			Payload: timestamppb.New(et),
		}
	}
	cc := &captureClient{}
	p, _ := bus.NewProducer(cc, bus.ProducerConfig{Source: "s/1", ProducerVersion: "v", Tenant: "cfg-tenant"})
	lastTenant := func() string {
		env, _, _ := bus.Unframe(cc.sent[len(cc.sent)-1].Body)
		return env.TenantId
	}

	if err := p.Publish(context.Background(), mk()); err != nil {
		t.Fatal(err)
	}
	if got := lastTenant(); got != "cfg-tenant" {
		t.Fatalf("config fallback: tenant=%q want cfg-tenant", got)
	}

	if err := p.Publish(bus.WithTenantID(context.Background(), "ctx-tenant"), mk()); err != nil {
		t.Fatal(err)
	}
	if got := lastTenant(); got != "ctx-tenant" {
		t.Fatalf("ctx beats config: tenant=%q want ctx-tenant", got)
	}

	e := mk()
	e.TenantID = "explicit-tenant"
	if err := p.Publish(bus.WithTenantID(context.Background(), "ctx-tenant"), e); err != nil {
		t.Fatal(err)
	}
	if got := lastTenant(); got != "explicit-tenant" {
		t.Fatalf("explicit beats ctx+config: tenant=%q want explicit-tenant", got)
	}
}

// TestConsumerPropagatesTenant proves the consumer stashes the inbound tenant
// onto ctx so a derived publish in the handler inherits it (MT-01b), and that a
// pre-tenancy event (no tenant_id) maps to SystemTenant on the replay path.
func TestConsumerPropagatesTenant(t *testing.T) {
	cases := []struct {
		name      string
		inbound   string
		replay    bool
		wantOnCtx string
	}{
		{"live tenant propagates", "globex", false, "globex"},
		{"pre-tenancy replay maps to system", "", true, bus.SystemTenant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope()
			env.TenantId = tc.inbound
			validator := bus.Validate
			if tc.replay {
				env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
				validator = bus.ValidateReplay
			}
			sub := &oneShotSub{msg: bus.Message{Subject: "x", Body: frame(t, env, []byte("p"))}}
			c, err := bus.NewConsumer(sub, bus.WithValidator(validator))
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if err := c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
				got = bus.TenantIDFromContext(ctx)
				return nil
			}); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			if got != tc.wantOnCtx {
				t.Fatalf("ctx tenant=%q want %q", got, tc.wantOnCtx)
			}
		})
	}
}
