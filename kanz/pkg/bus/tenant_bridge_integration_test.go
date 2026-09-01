package bus_test

// MT-02 (#358): AN ORDER FOR TENANT T REACHES T'S OMS, AND NO OTHER TENANT'S.
//
// This runs against the REAL committed accounts — test/mtls/up.sh extracts
// tenants.conf straight out of infra/nats/tenancy.yaml — with real SVIDs mapped
// by verify_and_map, so what is proven here is the configuration that ships,
// not a fixture written to agree with the test.
//
// A fake bus cannot prove any of it. Account isolation IS the property, and a
// double has no accounts: it would deliver every publish to every subscriber and
// report success. Measured on a throwaway two-account broker before this was
// written:
//
//	cross-account (gw -> oms): NONE  <- accounts isolate
//	same-account control     : DELIVERED
//
// THE TEST IS A PAIR, in both directions. One subscriber can only ever show
// delivery; isolation needs the negative half. So an order for `acme` must reach
// oms-acme and NOT the platform OMS, and the platform tenant's own order must
// reach the platform OMS and NOT oms-acme.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

// dialAs opens a raw NATS connection under one SVID, for the SUBSCRIBING side.
// Core subscriptions see JetStream publishes (a JS publish is a core publish the
// stream also captures), and they keep the consumers free of per-account stream
// provisioning, which is a separate concern.
func dialAs(t *testing.T, url, certDir, identity string) *nats.Conn {
	t.Helper()
	tlsCfg := transport.ClientTLSConfig(sourceFromDir(t, certDir, identity), transport.AuthorizeMesh())
	conn, err := nats.Connect(url, nats.Secure(tlsCfg), nats.Name("bridge-it-"+identity))
	if err != nil {
		t.Fatalf("connect as %s: %v\n\n"+
			"tenancy.yaml must admit this SVID; an identity it does not list maps to NO account "+
			"and is refused outright (SEC-M3).", identity, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func subscribeLogical(t *testing.T, conn *nats.Conn, subject string) *nats.Subscription {
	t.Helper()
	sub, err := conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// received drains one message within d, or reports none.
func received(t *testing.T, sub *nats.Subscription, d time.Duration) (string, bool) {
	t.Helper()
	msg, err := sub.NextMsg(d)
	if err != nil {
		return "", false
	}
	return string(msg.Data), true
}

func TestAnOrderReachesOnlyItsOwnTenantsOMS(t *testing.T) {
	url, certDir := mtlsEnv(t)

	// Three identities, three roles, exactly as production wires them:
	// api-gateway publishes in __system__; oms subscribes in __system__;
	// oms-acme subscribes in the `acme` account.
	systemOMS := dialAs(t, url, certDir, "systemoms")
	tenantOMS := dialAs(t, url, certDir, "tenantoms")

	// BOTH consumers subscribe the UNCHANGED logical subject. Neither knows a
	// routing prefix exists — that is what the per-account import buys.
	const logical = "order.order.submit"
	sysSub := subscribeLogical(t, systemOMS, logical)
	acmeSub := subscribeLogical(t, tenantOMS, logical)

	// The PUBLISHING side is the real one: bus.Producer over JetStream, exactly
	// as the gateway's composition root builds it. That matters more than it
	// looks — the producer publishes through JetStream, so a subject no stream
	// covers comes back "nats: no response from stream". A core-publish test
	// would route correctly and hide that the gateway could not publish at all.
	producer := producerAs(t, url, certDir, "gateway", "api-gateway")

	// --- an order for `acme` -------------------------------------------------
	publishOrder(t, producer, "acme", "acme-order")

	if got, ok := received(t, acmeSub, 5*time.Second); !ok || !strings.Contains(got, "acme-order") {
		t.Fatalf("oms-acme did not receive the order (got %q, ok=%v).\n\n"+
			"This is the defect #358 exists to close: the gateway holds one SVID and therefore "+
			"one account, and NATS routes on subject — it cannot dispatch on the envelope's "+
			"tenant_id. Without the export on __system__ and the import on `acme`, the tenant's "+
			"OMS authenticates, subscribes, and receives nothing at all.", got, ok)
	}
	if got, ok := received(t, sysSub, 500*time.Millisecond); ok {
		t.Fatalf("the PLATFORM OMS received a tenant's order: %q.\n\n"+
			"Isolation is inverted. The receiving OMS cannot detect this — its RLS pool is scoped "+
			"by its own tenant while the envelope carries another's.", got)
	}

	// --- the platform tenant's own order ------------------------------------
	//
	// The non-vacuous half of the pair. Without it, a broker that dropped
	// EVERYTHING would pass the assertions above.
	publishOrder(t, producer, "__system__", "system-order")

	if got, ok := received(t, sysSub, 5*time.Second); !ok || !strings.Contains(got, "system-order") {
		t.Fatalf("the platform OMS did not receive its own order (got %q, ok=%v).\n\n"+
			"The gateway prefixes unconditionally, so __system__ must map its own prefix back to "+
			"the logical subject. Without that mapping the OMS running in production today stops "+
			"receiving orders the moment this ships — with no error on either side.", got, ok)
	}
	if got, ok := received(t, acmeSub, 500*time.Millisecond); ok {
		t.Fatalf("oms-acme received the platform tenant's order: %q", got)
	}
}

// producerAs builds the bus.Producer a composition root would, under one SVID.
func producerAs(t *testing.T, url, certDir, identity, source string) *bus.Producer {
	t.Helper()
	tlsCfg := transport.ClientTLSConfig(sourceFromDir(t, certDir, identity), transport.AuthorizeMesh())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bridge-it-" + identity, TLSConfig: tlsCfg})
	if err != nil {
		t.Fatalf("dial as %s: %v", identity, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	p, err := bus.NewProducer(client, bus.ProducerConfig{Source: source, ProducerVersion: "bridge-it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	return p
}

// publishOrder mints one SubmitOrder exactly as orders.Handler.publish does:
// the WIRE subject carries the tenant, the event_type stays the logical name.
func publishOrder(t *testing.T, p *bus.Producer, tenant, orderID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := p.Publish(ctx, bus.Event{
		Subject:        "tenant." + tenant + ".order.order.submit",
		EventType:      "order.order.submit",
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         "order",
		EventTime:      time.Now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: "idem-" + orderID,
		TenantID:       tenant,
		Payload:        &orderpb.SubmitOrder{OrderId: orderID},
	})
	if err != nil {
		t.Fatalf("publish for tenant %s: %v\n\n"+
			"`no response from stream` means __system__ has no stream bound to tenant.*.order.> — "+
			"the gateway would fail EVERY order for a real tenant.", tenant, err)
	}
}
