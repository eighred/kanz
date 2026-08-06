// THE ORDER COMMAND'S ENVELOPE, AND THE AUTH-01c GUARD, DRIVEN THROUGH A REAL
// PRODUCER (#245).
//
// orders_test.go's fakePub is an EVENT-LEVEL double: it records the bus.Event
// and returns nil. Everything that makes a command legal happens below that —
// bus.Producer stamps the envelope, runs Validate, MARSHALS the payload, and only
// then extracts CommandMetadata.issuer for VerifyCommandIssuer. A double at that
// layer removes all of it, which is why #245 listed this package with "the
// AUTH-01c forged-issuer guard is never exercised through the handler".
//
// So these tests build a REAL bus.Producer over a fake bus.Client, wired with
// auth.VerifyCommandIssuer exactly as cmd/api-gateway/main.go:259 wires it, and
// drive the REAL authz.Mux. The assertions are made on the WIRE BYTES rather
// than on the Event struct: the anti-forgery override is only worth anything if
// it survives marshalling, and the struct is not what leaves the process.
//
// A NOTE ON WHAT THE HANDLER CANNOT REACH, because it changes what "exercised"
// can mean here. bindMetadata OVERWRITES any client-supplied issuer with
// "user:"+principal.Subject, and VerifyIssuer accepts any issuer whose id-part
// equals the principal's Subject. The handler then puts THAT SAME principal on
// ctx. So layer 1 makes layer 2 structurally unable to fail on this path — a
// forged issuer cannot survive as far as the guard, because it was replaced
// before it got there. That is defence in depth working, not a gap; the guard
// exists for producers that do NOT override, and for a future handler that
// forgets to. TestTheForgedIssuerGuardIsLiveOnThisProducer therefore drives the
// guard directly rather than pretending an HTTP request can trip it.
package orders

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// realProducer mirrors cmd/api-gateway/main.go's producer, including the
// VerifyCommandIssuer wiring — the field whose absence would make every
// assertion below pass while the guard did nothing.
func realProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:              "api-gateway",
		ProducerVersion:     "test",
		Tenant:              "acme",
		VerifyCommandIssuer: auth.VerifyCommandIssuer,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

func submitBody(issuer string) []byte {
	body := &orderpb.SubmitOrder{
		OrderId:      "o1",
		PortfolioId:  "pf1",
		InstrumentId: "AAPL",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   &commonpb.Decimal{Coefficient: 1025, Exponent: -2},
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		Metadata:     &commandpb.CommandMetadata{Issuer: issuer, TargetId: "o1"},
	}
	raw, _ := protojson.Marshal(body)
	return raw
}

// THE FORGED ISSUER IS OVERRIDDEN, AND THAT IS PROVEN ON THE BYTES.
//
// orders_test.go asserts the override on the Event struct the double captured.
// This asserts it on the payload the producer actually marshalled and handed to
// the transport, with Validate and the AUTH-01c extraction live in the path —
// which is the only version of the claim that says anything about production.
func TestSubmitOverridesAForgedIssuerOnTheWire(t *testing.T) {
	prod, cc := realProducer(t)
	h := New(prod)
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(string(submitBody("user:attacker"))))
	req.Header.Set("Idempotency-Key", "idem-123")
	req = authed(req, "alice", "acme")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}

	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("the emitted command fails Validate: %v", err)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Errorf("event_class = %v, want COMMAND", got)
	}
	if got := env.GetTenantId(); got != "acme" {
		t.Errorf("tenant_id = %q, want acme", got)
	}

	var sent orderpb.SubmitOrder
	if err := proto.Unmarshal(payload, &sent); err != nil {
		t.Fatalf("unmarshal the marshalled payload: %v", err)
	}
	if got := sent.GetMetadata().GetIssuer(); got != "user:alice" {
		t.Fatalf("issuer on the wire = %q, want user:alice. The client sent user:attacker and the "+
			"gateway is the sole issuer authority — an override that does not survive marshalling "+
			"is not an override", got)
	}
	if got := sent.GetOrderId(); got != "o1" {
		t.Errorf("order_id = %q — the payload was rewritten by something other than the issuer bind", got)
	}
}

// THE GUARD IS LIVE ON THIS PRODUCER, and refuses an issuer the principal may
// not assume.
//
// Driven directly rather than through the handler, and the package doc says why:
// bindMetadata replaces the issuer with one derived from the SAME principal that
// goes on ctx, so no HTTP request can present the guard with a mismatch. This is
// the test that fails if VerifyCommandIssuer is ever dropped from the gateway's
// ProducerConfig — the single line that would silently disarm AUTH-01c while
// every handler test kept passing.
func TestTheForgedIssuerGuardIsLiveOnThisProducer(t *testing.T) {
	prod, cc := realProducer(t)

	// A principal who is NOT the issuer, and holds no delegation claim for it.
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Tenant: "acme"})

	err := prod.Publish(ctx, bus.Event{
		Subject:        subjectSubmit,
		EventType:      subjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         "order",
		EventTime:      time.Now().UTC(),
		PartitionKey:   "o1",
		IdempotencyKey: "idem-1",
		TenantID:       "acme",
		Payload: &orderpb.SubmitOrder{
			OrderId:  "o1",
			Metadata: &commandpb.CommandMetadata{Issuer: "user:mallory", TargetId: "o1"},
		},
	})
	if err == nil {
		t.Fatal("a command declaring an issuer the principal may not assume was PUBLISHED. AUTH-01c " +
			"is not armed on this producer, and the gateway would forward a forged issuer onto the " +
			"capital path")
	}
	if !errors.Is(err, auth.ErrForgedIssuer) {
		t.Errorf("refused, but not as forgery: %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused command still reached the transport (%d message(s))", len(cc.sent))
	}
}

// A DELEGATED ISSUER IS ALLOWED. The mirror of the test above: without it, a
// guard that refused EVERYTHING would pass the forgery case and break every
// legitimate strategy-issued command.
func TestADelegatedIssuerIsAccepted(t *testing.T) {
	prod, cc := realProducer(t)

	// []any, NOT []string: claims arrive decoded from a JWT, where a JSON array
	// unmarshals to []any, and stringListClaim handles []any and string only. A
	// []string here would silently yield no issuers and this test would "prove"
	// delegation is refused — passing the forgery case above for the wrong reason.
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		Subject: "alice", Tenant: "acme",
		Claims: map[string]any{auth.ClaimIssuers: []any{"strategy:momentum-v2"}},
	})

	err := prod.Publish(ctx, bus.Event{
		Subject:        subjectSubmit,
		EventType:      subjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         "order",
		EventTime:      time.Now().UTC(),
		PartitionKey:   "o1",
		IdempotencyKey: "idem-2",
		TenantID:       "acme",
		Payload: &orderpb.SubmitOrder{
			OrderId:  "o1",
			Metadata: &commandpb.CommandMetadata{Issuer: "strategy:momentum-v2", TargetId: "o1"},
		},
	})
	if err != nil {
		t.Fatalf("a DELEGATED issuer was refused: %v. The claim exists so a principal can issue as a "+
			"strategy it operates; a guard that refuses those refuses the whole feature", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
}
