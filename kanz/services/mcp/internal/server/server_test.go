package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/pkg/auth"
)

// THE READ PLANE'S BOUNDARIES, TESTED RATHER THAN DECLARED (#743).
//
// This surface exists under three non-negotiable boundaries: it never receives,
// exposes, stores or proxies a venue credential; it never exposes order
// placement, execution, cancel or amend; and it is not the venue transport. The
// first and third are structural — nothing here imports an exchange or a venue
// client, and test/arch asserts that on the import graph, which is the only
// place it can be asserted honestly.
//
// The second is what these tests carry: every capability this plane offers is a
// read, every one of them goes through the shared gate first, and a refusal
// tells the caller nothing about another tenant's state.

type fakeReader struct {
	measures map[string]float64
	err      error
	reads    int
}

func (f *fakeReader) Measures(_ context.Context, _ string) (map[string]float64, error) {
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	return f.measures, nil
}

type fakeOwner struct {
	byID map[string]string
	err  error
}

func (f fakeOwner) OwnerTenant(_ context.Context, id string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	t, ok := f.byID[id]
	if !ok {
		return "", agentgate.ErrResourceNotVisible
	}
	return t, nil
}

func testPolicy() *auth.Policy {
	return &auth.Policy{Roles: map[string][]auth.Action{
		"analyst": {auth.ActionRiskRead, auth.ActionRiskScenario},
	}}
}

func newServer(t *testing.T, owner agentgate.OwnerResolver, reader Reader) *Server {
	t.Helper()
	rd := &Readiness{}
	rd.Set(true)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := agentgate.NewGate(auth.NewPolicyAuthorizer(testPolicy()), owner, quiet)
	return New(rd, gate, reader, quiet)
}

// rpc posts a JSON-RPC request as an authenticated caller in tenant t1.
func rpc(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return rpcAs(t, s, body, "user:pm", "t1")
}

func rpcAs(t *testing.T, s *Server, body, subject, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	auth.SetPrincipalHeaders(req.Header, subject, tenant, []string{"analyst"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decodeRPC(t *testing.T, rec *httptest.ResponseRecorder) response {
	t.Helper()
	var r response
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v — body %s", err, rec.Body.String())
	}
	return r
}

func healthyServer(t *testing.T) (*Server, *fakeReader) {
	t.Helper()
	reader := &fakeReader{measures: map[string]float64{"VaR99": 1250000}}
	return newServer(t, fakeOwner{byID: map[string]string{"PF-1": "t1"}}, reader), reader
}

// THE PRINCIPAL IS REQUIRED, and it is required before the envelope is parsed.
// The gateway is this platform's sole identity authority; a surface that
// authenticated its own callers would be a second one.
func TestRPC_WithoutAPrincipalIsRefused(t *testing.T) {
	s, _ := healthyServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 — every tool here is tenant-scoped and there is nothing to scope by", rec.Code)
	}
}

// EVERY DECLARED CAPABILITY IS A READ. This is #743's second boundary, and the
// list is built from the same slice the dispatcher runs, so a tool cannot appear
// here and be uncallable, or be callable and not appear.
func TestToolsList_OffersOnlyReads(t *testing.T) {
	s, _ := healthyServer(t)
	res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	raw, _ := json.Marshal(res.Result)
	var list toolsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode tools list: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Fatal("no tools offered — this test would pass vacuously")
	}
	// A name suggesting a capital action must never appear. Checked by substring
	// rather than an allow-list so a tool added later is caught by what it CLAIMS
	// to do, not by whether somebody remembered to extend a list.
	forbidden := []string{"submit", "order", "cancel", "amend", "place", "execute", "trade", "venue", "credential", "key"}
	for _, tool := range list.Tools {
		lower := strings.ToLower(tool.Name)
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Errorf("tool %q names a capital or venue action — this plane is read-only (#743)", tool.Name)
			}
		}
	}
}

// The handshake STATES read-only rather than leaving it to be inferred from the
// absence of write capabilities. MCP reports capability by omission, which is
// the two-state trap #675 removed from this estate.
func TestInitialize_StatesReadOnlyRatherThanImplyingIt(t *testing.T) {
	s, _ := healthyServer(t)
	res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`))

	raw, _ := json.Marshal(res.Result)
	var got initializeResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if !got.Capabilities.ReadOnly {
		t.Error("capabilities does not state readOnly — a reader would have to conclude it from " +
			"silence, which is exactly what 'empty means DID NOT SAY' forbids")
	}
	if got.Capabilities.Tools.ListChanged {
		t.Error("listChanged is true — a plane whose tool set can grow at runtime is one whose " +
			"boundaries are decided at runtime")
	}
}

// A tool call is GATED, and a cross-tenant probe learns nothing. The message is
// the gate's closed-set rendering; the authorizer's reason names the owning
// tenant and must not appear.
func TestToolsCall_CrossTenantIsRefusedAndLeaksNothing(t *testing.T) {
	s, reader := healthyServer(t)
	rec := rpcAs(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"get_risk_measures","arguments":{"portfolio_id":"PF-1"}}}`, "user:mallory", "t2")

	res := decodeRPC(t, rec)
	if res.Error == nil {
		t.Fatalf("a t2 caller reached a t1 portfolio: %+v", res.Result)
	}
	if reader.reads != 0 {
		t.Fatalf("the read surface was called %d time(s) behind a refusal — the refusal is cosmetic", reader.reads)
	}
	if res.Error.Code != codeUnauthorized {
		t.Errorf("code = %d, want %d", res.Error.Code, codeUnauthorized)
	}
	for _, leak := range []string{"t1", "tenant", "cross"} {
		if strings.Contains(res.Error.Message, leak) {
			t.Errorf("refusal %q carries %q — the authorizer's reason must not reach an agent", res.Error.Message, leak)
		}
	}
	if res.Error.Message != agentgate.NotVisible(auth.ResourcePortfolio, "PF-1") {
		t.Errorf("message = %q, want the gate's not-visible wording", res.Error.Message)
	}
}

// A resource that does not exist is answered in the SAME words as one owned by
// another tenant, or the difference enumerates another tenant's portfolios.
func TestToolsCall_UnknownAndCrossTenantAreIndistinguishable(t *testing.T) {
	s, _ := healthyServer(t)
	call := func(id string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_risk_measures","arguments":{"portfolio_id":"` + id + `"}}}`
	}
	existing := decodeRPC(t, rpcAs(t, s, call("PF-1"), "user:mallory", "t2"))
	missing := decodeRPC(t, rpcAs(t, s, call("PF-NOPE"), "user:mallory", "t2"))

	if existing.Error == nil || missing.Error == nil {
		t.Fatal("both probes must be refused")
	}
	a := strings.TrimSuffix(existing.Error.Message, "PF-1")
	b := strings.TrimSuffix(missing.Error.Message, "PF-NOPE")
	if a != b {
		t.Fatalf("an existing portfolio answers %q and a missing one answers %q — the difference is "+
			"a cross-tenant existence oracle", existing.Error.Message, missing.Error.Message)
	}
}

// An ownership lookup that cannot answer FAILS CLOSED and reads nothing.
func TestToolsCall_UnresolvedOwnerFailsClosed(t *testing.T) {
	reader := &fakeReader{measures: map[string]float64{"VaR99": 1}}
	s := newServer(t, fakeOwner{err: errors.New("deadline exceeded")}, reader)

	res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"get_risk_measures","arguments":{"portfolio_id":"PF-1"}}}`))
	if res.Error == nil {
		t.Fatal("an unresolvable owner was served — availability decided isolation")
	}
	if reader.reads != 0 {
		t.Fatalf("read surface called %d time(s) behind a failed ownership lookup", reader.reads)
	}
}

// NON-VACUITY: the entitled caller is served. Without this, every refusal above
// is satisfied by a plane that refuses everything.
func TestToolsCall_EntitledCallerIsServed(t *testing.T) {
	s, reader := healthyServer(t)
	res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"get_risk_measures","arguments":{"portfolio_id":"PF-1"}}}`))

	if res.Error != nil {
		t.Fatalf("entitled caller refused: %+v", res.Error)
	}
	if reader.reads != 1 {
		t.Fatalf("reads = %d, want 1", reader.reads)
	}
	raw, _ := json.Marshal(res.Result)
	if !strings.Contains(string(raw), "VaR99") {
		t.Errorf("result does not carry the measure: %s", raw)
	}
}

// A METHOD THIS PLANE DOES NOT SERVE IS NAMED AS ABSENT. MCP defines write-side
// methods; answering them "method not found" states they are absent by decision
// rather than leaving a caller to wonder whether it asked wrongly.
func TestRPC_AWriteSideMethodIsNotServed(t *testing.T) {
	s, reader := healthyServer(t)
	for _, method := range []string{"tools/write", "resources/write", "completion/complete", "sampling/createMessage"} {
		res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
		if res.Error == nil || res.Error.Code != codeMethodNotFound {
			t.Errorf("%s: got %+v, want method-not-found", method, res.Error)
		}
	}
	if reader.reads != 0 {
		t.Errorf("an unserved method reached the read surface %d time(s)", reader.reads)
	}
}

// The envelope is validated before any method runs, and the id is echoed back
// UNCHANGED — the spec permits a string, a number or null, and a caller that
// asked with "abc" must not be answered about 0.
func TestRPC_EnvelopeAndIDHandling(t *testing.T) {
	s, _ := healthyServer(t)

	if res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`)); res.Error == nil ||
		res.Error.Code != codeInvalidRequest {
		t.Errorf("a wrong jsonrpc version was accepted: %+v", res.Error)
	}
	if res := decodeRPC(t, rpc(t, s, `not json`)); res.Error == nil || res.Error.Code != codeParseError {
		t.Errorf("malformed JSON was accepted: %+v", res.Error)
	}
	res := decodeRPC(t, rpc(t, s, `{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`))
	if string(res.ID) != `"abc"` {
		t.Errorf("id echoed as %s, want \"abc\" — a string id must come back a string", res.ID)
	}
}
