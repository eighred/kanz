package orders

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

type fakePub struct{ last *bus.Event }

func (f *fakePub) Publish(_ context.Context, e bus.Event) error {
	f.last = &e
	return nil
}

func authed(req *http.Request, sub, tenant string) *http.Request {
	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: sub, Tenant: tenant, Roles: []string{"trader"}})
	return req.WithContext(ctx)
}

func TestSubmit_Unauthenticated_401(t *testing.T) {
	h := New(&fakePub{})
	rr := httptest.NewRecorder()
	h.Routes(http.NewServeMux())
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{}`))
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.ServeHTTP(rr, req) // no principal on ctx
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestSubmit_BindsIssuerAndTenant(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	mux := http.NewServeMux()
	h.Routes(mux)

	// Body carries a FORGED issuer the gateway must override.
	body := &orderpb.SubmitOrder{
		OrderId:      "o1",
		PortfolioId:  "pf1",
		InstrumentId: "AAPL",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   &commonpb.Decimal{Coefficient: 1025, Exponent: -2},
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		Metadata:     &commandpb.CommandMetadata{Issuer: "user:attacker", TargetId: "o1"},
	}
	raw, _ := protojson.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(string(raw)))
	req.Header.Set("Idempotency-Key", "idem-123")
	req = authed(req, "alice", "acme")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	if pub.last == nil {
		t.Fatal("no event published")
	}
	e := pub.last
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Fatalf("class = %v, want COMMAND", e.EventClass)
	}
	if e.EventType != subjectSubmit || e.PartitionKey != "o1" {
		t.Fatalf("type/partition = %q/%q", e.EventType, e.PartitionKey)
	}
	if e.IdempotencyKey != "idem-123" || e.TenantID != "acme" {
		t.Fatalf("idem/tenant = %q/%q, want idem-123/acme", e.IdempotencyKey, e.TenantID)
	}
	cmd := e.Payload.(*orderpb.SubmitOrder)
	if got := cmd.GetMetadata().GetIssuer(); got != "user:alice" {
		t.Fatalf("issuer = %q, want user:alice (forged value must be overridden)", got)
	}
}

func TestSubmit_GeneratesOrderID(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/orders",
		strings.NewReader(`{"portfolioId":"pf1","instrumentId":"AAPL","side":"SIDE_BUY","quantity":{"coefficient":"100","exponent":0},"orderType":"ORDER_TYPE_MARKET","timeInForce":"TIME_IN_FORCE_DAY"}`))
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	cmd := pub.last.Payload.(*orderpb.SubmitOrder)
	if cmd.GetOrderId() == "" {
		t.Fatal("order_id not generated")
	}
	if cmd.GetMetadata().GetTargetId() != cmd.GetOrderId() {
		t.Fatalf("target_id %q != order_id %q", cmd.GetMetadata().GetTargetId(), cmd.GetOrderId())
	}
}

func TestSubmit_WritesDisabled_503(t *testing.T) {
	h := New(nil) // no publisher
	mux := http.NewServeMux()
	h.Routes(mux)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{"orderId":"o1"}`)), "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
