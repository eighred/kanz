package authz_test

// EVERY CAPABILITY DECISION IS RECORDED (AUTH-01d, #352).
//
// The gateway is the platform's SOLE identity authority, and it recorded its
// allows and denies nowhere at all — not to the observation stream, not even to
// a log. Every other authorization surface in the estate had at least an slog
// recorder. So `POST /v1/orders` was refused, or permitted, and afterwards there
// was no way to establish which had happened, to whom, or under what authority.
//
// These tests pin the four properties that make the record worth having:
// both verdicts are recorded, the reason NAMES the granting role, the resource
// is the route pattern, and a request that arrives with no principal — a
// composed-wrong middleware chain, indistinguishable from an ordinary 403 at the
// edge — is recorded too.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

type capturingRecorder struct {
	mu  sync.Mutex
	got []*observationpb.DecisionLog
}

func (c *capturingRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, e)
	return nil
}

func (c *capturingRecorder) entries() []*observationpb.DecisionLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*observationpb.DecisionLog(nil), c.got...)
}

// serveRecorded runs one request through a mux wired to a capturing recorder and
// returns the status plus whatever was recorded. roles == nil means the request
// carries NO principal.
func serveRecorded(t *testing.T, method, path string, roles []string) (int, []*observationpb.DecisionLog) {
	t.Helper()
	rec := &capturingRecorder{}
	m := authz.NewMux(grants(), rec)
	m.Handle(authz.Read, "GET /v1/portfolios/{id}/exposure", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m.Handle(authz.Trade, "POST /v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if roles != nil {
		req = req.WithContext(middleware.WithPrincipal(req.Context(),
			&middleware.Principal{Subject: "user-1", Tenant: "acme", Roles: roles}))
	}
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	return rr.Code, rec.entries()
}

func only(t *testing.T, entries []*observationpb.DecisionLog) *observationpb.DecisionLog {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("recorded %d decisions, want exactly 1", len(entries))
	}
	return entries[0]
}

// A REFUSAL IS RECORDED — the case everyone expects.
func TestADeniedCapabilityIsRecorded(t *testing.T) {
	code, entries := serveRecorded(t, http.MethodPost, "/v1/orders", []string{roleReader})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — this test is asserting on the wrong path", code)
	}
	e := only(t, entries)

	attrs := e.GetAttributes()
	if attrs["decision"] != "deny" {
		t.Errorf("decision = %q, want deny", attrs["decision"])
	}
	if attrs["principal.subject"] != "user-1" {
		t.Errorf("principal.subject = %q, want user-1", attrs["principal.subject"])
	}
	if attrs["action"] != string(authz.Trade) {
		t.Errorf("action = %q, want %q", attrs["action"], authz.Trade)
	}
	if attrs["resource.id"] != "POST /v1/orders" {
		t.Errorf("resource.id = %q, want the ROUTE PATTERN \"POST /v1/orders\" — the concrete "+
			"path would put URL ids in the audit trail and still not say what was checked",
			attrs["resource.id"])
	}
}

// AN ALLOW IS RECORDED TOO, and this is the half that gets dropped as "noise".
//
// A trail holding only refusals cannot answer who DID submit the order, which is
// the question an investigation actually asks. pkg/auth/audit.go states the
// contract as "every allow AND deny"; services/audit takes the same line for the
// same reason ("audit completeness over economy").
func TestAnAllowedCapabilityIsRecorded(t *testing.T) {
	code, entries := serveRecorded(t, http.MethodPost, "/v1/orders", []string{roleTrader})
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — this test is asserting on the wrong path", code)
	}
	e := only(t, entries)

	if got := e.GetAttributes()["decision"]; got != "allow" {
		t.Fatalf("decision = %q, want allow.\n\n"+
			"Permitted access is not being recorded, so the stream can show that a trade was "+
			"refused and never that one was made.", got)
	}
}

// THE REASON NAMES THE GRANTING ROLE.
//
// "allowed" is not reconstructable a year later: the grants map will have
// changed, and the question is which role carried the capability AT THE TIME.
func TestTheRecordedReasonNamesTheGrantingRole(t *testing.T) {
	_, entries := serveRecorded(t, http.MethodPost, "/v1/orders", []string{roleReader, roleTrader})
	e := only(t, entries)

	if reason := e.GetAttributes()["reason"]; !strings.Contains(reason, roleTrader) {
		t.Fatalf("reason = %q, want it to name %q.\n\n"+
			"The caller holds two roles and only one of them carries trade. A record that says "+
			"only \"allowed\" cannot answer which authority was exercised.", reason, roleTrader)
	}
}

// A REQUEST WITH NO PRINCIPAL IS RECORDED.
//
// This router runs inside middleware.Auth, so no principal means the chain was
// composed wrong. At the edge it is a 403 identical to an ordinary refusal — the
// audit trail is the only place that misconfiguration becomes visible.
func TestARequestWithNoPrincipalIsRecorded(t *testing.T) {
	code, entries := serveRecorded(t, http.MethodGet, "/v1/portfolios/p1/exposure", nil)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	e := only(t, entries)

	attrs := e.GetAttributes()
	if attrs["decision"] != "deny" {
		t.Errorf("decision = %q, want deny", attrs["decision"])
	}
	if _, ok := attrs["principal.subject"]; ok {
		t.Errorf("a principal.subject was recorded for a request that carried no principal: %q",
			attrs["principal.subject"])
	}
	if reason := attrs["reason"]; !strings.Contains(reason, "composed wrong") {
		t.Errorf("reason = %q — it should say the middleware chain is misconfigured, because "+
			"nothing else distinguishes this from an ordinary insufficient-capability 403", reason)
	}
}

// A NIL RECORDER IS THE TEST-ONLY SHAPE, and it must not panic the edge.
func TestANilRecorderStillServes(t *testing.T) {
	m := authz.NewMux(grants(), nil)
	m.Handle(authz.Read, "GET /v1/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil).WithContext(
		middleware.WithPrincipal(context.Background(),
			&middleware.Principal{Subject: "u", Tenant: "acme", Roles: []string{roleReader}}))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
