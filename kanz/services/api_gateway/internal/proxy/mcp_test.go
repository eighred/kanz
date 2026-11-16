package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE MCP READ PLANE HAS TO BE REACHABLE, AND REACHABILITY FAILS AT THREE LAYERS
// (#762).
//
// services/mcp shipped with a Dockerfile, a Deployment, a Service, a
// PodDisruptionBudget, a CI image build and a namespace quota — and no route
// here, no upstream address, and no NetworkPolicy in either direction. It was
// deployed and inert. The other two layers are manifest-side and are guarded by
// test/arch; this file holds the one that is code: the route exists, it demands
// a principal, and it forwards to the right upstream at the right path.

// A CALLER WITHOUT A PRINCIPAL IS REFUSED, AND THE ROUTE CARRIES ITS OWN CHECK.
//
// TWO ARMS, AND THE LIMIT OF THE SECOND ONE IS STATED RATHER THAN LEFT TO BE
// DISCOVERED. The first goes through the authz mux and proves the capability
// layer refuses an unauthenticated caller. The second calls handle directly and
// proves handle HONOURS requirePrincipal — a real property, and the one that
// backs handle's own doc: it gates "beyond the chain's auth middleware — so even
// an auth-disabled dev gateway never [forwards an unauthenticated call]".
//
// NEITHER ARM PROVES THE ROUTE PASSES true. authz.Mux always enforces the
// capability, which needs a principal, so the mux arm passes whatever the flag
// says — a mutation flipping the route to false survives both. There is no
// auth-disabled Mux to construct here, so the flag's only observable effect is
// unreachable from this package. That is worth knowing rather than papering
// over: the route's defence-in-depth is asserted by inspection, not by this
// file, and the layer that actually stops an unauthenticated caller in a
// configured gateway is the mux.
//
// It matters here more than on most routes because the plane downstream
// authenticates nobody and acts on whatever principal header it receives.
func TestMCP_UnauthenticatedIsRefusedAtTheEdge(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{"jsonrpc":"2.0"}`)}}
	h := New(be, Roles{})
	mux := testMux()
	h.Routes(mux)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("an unauthenticated caller reached the MCP plane: status=%d", rr.Code)
	}
	if be.last.Service == ServiceMCP {
		t.Fatal("the request was forwarded upstream before the principal was checked — the plane " +
			"trusts the header it is handed, so the edge is where an absent one has to stop")
	}

	// The route's OWN check, isolated from the mux: an auth-disabled gateway.
	be2 := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{"jsonrpc":"2.0"}`)}}
	h2 := New(be2, Roles{})
	direct := h2.handle(ServiceMCP, true, func(string) string { return "/mcp" })
	rr2 := httptest.NewRecorder()
	direct(rr2, httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(`{}`)))

	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("handler status = %d, want 401 — the route must demand a principal itself, not "+
			"only inherit the mux's refusal", rr2.Code)
	}
	if be2.last.Service == ServiceMCP {
		t.Fatal("an auth-disabled gateway forwarded an unauthenticated call to a plane that " +
			"authenticates nobody and reads the tenant off the header it is handed")
	}
}

// THE ROUTE FORWARDS TO mcp, AT THE PATH THAT PLANE ACTUALLY SERVES.
//
// The gateway mounts /v1/mcp; the plane serves POST /mcp. A rewrite that left
// /v1/mcp on the wire would 404 upstream, which reads as a broken plane rather
// than a misrouted gateway — the same shape as the :9000 port the Deployment
// carried, where both halves had to be wrong for the symptom to be silence.
func TestMCP_ForwardsToThePlaneAtItsOwnPath(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, ContentType: "application/json", Body: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)}}
	h := New(be, Roles{})
	mux := testMux()
	h.Routes(mux)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_risk_measures"}}`
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(body)), "agent-1", "t1")
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if be.last.Service != ServiceMCP {
		t.Fatalf("service = %q, want %q", be.last.Service, ServiceMCP)
	}
	if be.last.Path != "/mcp" {
		t.Fatalf("upstream path = %q, want /mcp — the plane serves POST /mcp and would 404 anything "+
			"else, which reads as a broken plane rather than a misrouted gateway", be.last.Path)
	}
	// THE PRINCIPAL IS THE WHOLE AUTHORIZATION INPUT on the other side: every
	// tool call is scoped to this tenant by the plane's own gate. A forwarded
	// request without it is one the plane refuses, and one with the WRONG tenant
	// is a cross-tenant read.
	if be.last.Principal == nil || be.last.Principal.Subject != "agent-1" || be.last.Principal.Tenant != "t1" {
		t.Fatalf("principal not forwarded: %+v", be.last.Principal)
	}
	// The JSON-RPC envelope must arrive byte-for-byte: the method and the tool
	// name live inside it, so a body the gateway reshaped is a different call.
	if string(be.last.Body) != body {
		t.Fatalf("body = %q, not forwarded verbatim", be.last.Body)
	}
}

// AN UNCONFIGURED PLANE 503s RATHER THAN 404s, which is the posture the whole
// estate uses for an upstream that is absent in this deployment: "not configured
// here" is true, "no such route" is not.
func TestMCP_UnconfiguredUpstreamIsUnavailableNotMissing(t *testing.T) {
	h := New(nil, Roles{}) // no backend ⇒ no upstream configured
	mux := testMux()
	h.Routes(mux)

	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(`{}`)), "u1", "t1")
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an unconfigured MCP upstream", rr.Code)
	}
}
