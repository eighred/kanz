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
	"github.com/kanz-eng/kanz/services/api-gateway/internal/authz"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

type fakePub struct{ last *bus.Event }

func (f *fakePub) Publish(_ context.Context, e bus.Event) error {
	f.last = &e
	return nil
}

// testMux is the router the gateway actually serves /v1 on (SEC-M2): every route declares a
// capability, and only a principal whose roles carry it gets through. The order routes
// require Trade, so these tests must hold a role that grants it — as a real caller must.
func testMux() *authz.Mux {
	return authz.NewMux(authz.Grants{"trader": {authz.Read, authz.Trade}})
}

func authed(req *http.Request, sub, tenant string) *http.Request {
	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: sub, Tenant: tenant, Roles: []string{"trader"}})
	return req.WithContext(ctx)
}

// TestSubmit_Unauthenticated_401 exercises the HANDLER'S OWN guard, by calling it directly
// rather than through the router. Both layers refuse an anonymous caller and both are
// deliberate: the router refuses because no principal carries the Trade capability (403 —
// see internal/authz), and the handler refuses because it will not build an order command
// with nobody to attribute it to (401). Defence in depth on the capital path is not
// redundancy; it is the point.
func TestSubmit_Unauthenticated_401(t *testing.T) {
	h := New(&fakePub{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{}`))
	h.submit(rr, req) // no principal on ctx
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestSubmit_BindsIssuerAndTenant(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	mux := testMux()
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
	mux := testMux()
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
	mux := testMux()
	h.Routes(mux)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{"orderId":"o1"}`)), "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// authedScoped authenticates a caller entitled to an explicit portfolio set.
func authedScoped(req *http.Request, sub, tenant string, portfolios ...string) *http.Request {
	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{
		Subject: sub, Tenant: tenant, Roles: []string{"trader"}, Portfolios: portfolios,
	})
	return req.WithContext(ctx)
}

// The gateway knows WHO is calling; the OMS holds the order and knows which
// portfolio it belongs to. Neither can authorize a cancel alone, so the caller's
// entitlement must travel on the command. If the gateway does not stamp it, the
// OMS's deny-by-default guard refuses every cancel — including legitimate ones.
func TestCancel_CarriesThePrincipalsPortfolioScope(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/cancel", nil)
	req = authedScoped(req, "alice", "acme", "pf1", "pf7")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	cmd := pub.last.Payload.(*orderpb.CancelOrder)
	got := cmd.GetMetadata().GetPrincipalPortfolios()
	if len(got) != 2 || got[0] != "pf1" || got[1] != "pf7" {
		t.Fatalf("principal_portfolios = %v, want [pf1 pf7] — without this the OMS cannot authorize the cancel", got)
	}
}

// The scope must come from the AUTHENTICATED principal, never from client input
// — otherwise the caller simply grants themselves the portfolio they are
// attacking, and the whole check is theatre.
func TestCancel_IgnoresClientSuppliedPortfolioScope(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	mux := testMux()
	h.Routes(mux)

	// A forged body claiming entitlement to someone else's portfolio.
	forged := &orderpb.CancelOrder{
		OrderId:  "o1",
		Metadata: &commandpb.CommandMetadata{PrincipalPortfolios: []string{"pf-victim"}},
	}
	raw, _ := protojson.Marshal(forged)
	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/cancel", strings.NewReader(string(raw)))
	req = authedScoped(req, "mallory", "acme", "pf1")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	got := pub.last.Payload.(*orderpb.CancelOrder).GetMetadata().GetPrincipalPortfolios()
	for _, p := range got {
		if p == "pf-victim" {
			t.Fatalf("principal_portfolios = %v — a CLIENT-SUPPLIED portfolio survived into the command", got)
		}
	}
	if len(got) != 1 || got[0] != "pf1" {
		t.Fatalf("principal_portfolios = %v, want [pf1] (the authenticated scope)", got)
	}
}

// A TOKEN THAT AUTHENTICATES BUT NAMES NO TENANT MUST NOT MOVE CAPITAL.
//
// This is not hypothetical. Neither authenticator requires the tenant claim —
// pkg/auth/oidc.go reads stringClaim(custom[cfg.TenantClaim]) and the dev HS256
// path decodes `tenant`, and both accept a token that omits it, gating only on
// the subject. The handler then stamps Event.TenantID from that principal, so
// before this guard an order command reached the producer with an empty tenant
// and bus.Validate rejected it: every submit and cancel failed 502, for every
// caller, if API_GATEWAY_OIDC_TENANT_CLAIM named a claim the IdP did not
// populate. Nothing here caught it, because every other test in this file hands
// authed() a non-empty tenant.
//
// The assertion that pub.last stays nil is the load-bearing half: refusing with
// the right status while still publishing would be the same defect wearing a
// better error code.
func TestSubmit_AuthenticatedWithoutTenant_403(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{}`))
	h.submit(rr, authed(req, "user-1", "")) // authenticated, no tenant claim

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an authenticated caller with no tenant cannot say "+
			"whose capital the order would spend, and must be refused rather than have one guessed", rr.Code)
	}
	if pub.last != nil {
		t.Fatalf("an order was published for a principal with no tenant (%+v). The producer would "+
			"reject it as tenant_id required, and a producer fallback would be worse still — it would "+
			"book somebody's order against the gateway's own tenant, permanently", pub.last)
	}
}

func TestCancel_AuthenticatedWithoutTenant_403(t *testing.T) {
	pub := &fakePub{}
	h := New(pub)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o-1/cancel", strings.NewReader(`{}`))
	h.cancel(rr, authed(req, "user-1", ""))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a cancel withdraws a live order and carries the same "+
			"authority as placing one", rr.Code)
	}
	if pub.last != nil {
		t.Fatalf("a cancel was published for a principal with no tenant (%+v)", pub.last)
	}
}
