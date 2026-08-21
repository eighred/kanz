package orders

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// THE SECOND SIGNATURE ON A HELD ORDER (#539, #410).
//
// The OMS holds an order that exceeds the dual-control threshold and waits for an
// ApproveOrder command carrying the digest of what was proposed. Nothing could send
// one: under network-policies.yaml the gateway is the OMS's only permitted caller,
// and the gateway had no approve route — so a held order was held forever and the
// control read, from the outside, exactly like an outage.
//
// What is worth testing here is not that an approver can release an order. It is the
// pair of properties that make this a control rather than a second name for Trade:
//
//   - A TRADER CANNOT APPROVE. If Approve collapsed into Trade, the person whose
//     order was held would sign their own release and the audit trail would record
//     two signatures from one pair of hands.
//   - AN APPROVER CANNOT TRADE. The reverse direction is what keeps the approver off
//     the capital path; without it the second signatory is simply a third trader.
//
// Four-eyes itself — that the approver is a different PERSON from the proposer — is
// deliberately NOT asserted here. The OMS compares authenticated subjects and owns
// that rule with the tests that prove it; restating it at the edge would be the
// second implementation this repository keeps paying for.

const approverRole = "compliance"

// approveMux mirrors buildRouter's grant for API_GATEWAY_APPROVE_ROLE: Read and
// Approve, never Trade. An approver who cannot read the order they are signing for
// is signing something unread.
func approveMux() *authz.Mux {
	return authz.NewMux(authz.Grants{
		approverRole: {authz.Read, authz.Approve},
		"trader":     {authz.Read, authz.Trade},
		"analyst":    {authz.Read},
	}, nil)
}

func approveRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/orders/o1/approve",
		strings.NewReader(`{"digest":"sha256:beef"}`))
}

func asRole(req *http.Request, sub, role string) *http.Request {
	return req.WithContext(middleware.WithPrincipal(req.Context(),
		&middleware.Principal{Subject: sub, Tenant: "acme", Roles: []string{role}}))
}

func TestApproveRefusesATradeToken(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(approveRequest(), "alice", "trader"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a TRADE token released a held order: status = %d, want 403.\n"+
			"An approval capability the trader already holds is not a second signature — it is the "+
			"same person signing twice, recorded in the trail as two.", rr.Code)
	}
	if pub.last != nil {
		t.Fatalf("a refused caller still published %q — the refusal was after the effect", pub.last.EventType)
	}
}

func TestApproveRefusesAReadToken(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(approveRequest(), "bob", "analyst"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a READ token released a held order: status = %d, want 403", rr.Code)
	}
	if pub.last != nil {
		t.Fatalf("a refused caller still published %q", pub.last.EventType)
	}
}

// AN APPROVER CAN — without this the refusals above are satisfied by a route nobody
// can reach, which is the #535 shape and the whole defect #539 exists to end.
func TestApprovePublishesTheCommandBoundToTheApprover(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	req := approveRequest()
	req.Header.Set("Idempotency-Key", "idem-approve-1")
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(req, "carol", approverRole))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("an APPROVE token was refused: status = %d (%s)", rr.Code, rr.Body.String())
	}
	if pub.last == nil {
		t.Fatal("no event published — the approval reached nothing and the order stays held")
	}
	e := pub.last
	if e.EventType != subjectApprove {
		t.Errorf("event type = %q, want %q — the OMS subscribes to that subject and nothing else",
			e.EventType, subjectApprove)
	}
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Errorf("class = %v, want COMMAND", e.EventClass)
	}
	if e.PartitionKey != "o1" {
		t.Errorf("partition key = %q, want the order id — approval must be ordered against the order "+
			"it releases", e.PartitionKey)
	}
	if e.TenantID != "acme" || e.IdempotencyKey != "idem-approve-1" {
		t.Errorf("tenant/idem = %q/%q, want acme/idem-approve-1", e.TenantID, e.IdempotencyKey)
	}
	cmd, ok := e.Payload.(*orderpb.ApproveOrder)
	if !ok {
		t.Fatalf("payload = %T, want *orderpb.ApproveOrder", e.Payload)
	}
	// THE ISSUER IS THE APPROVER, taken from the authenticated principal. Dual
	// control over a forgeable identity is theatre — one person would propose as
	// alice and approve as bob without ever holding a second credential.
	if got := cmd.GetMetadata().GetIssuer(); got != "user:carol" {
		t.Errorf("issuer = %q, want user:carol", got)
	}
	if got := cmd.GetMetadata().GetTargetId(); got != "o1" {
		t.Errorf("target_id = %q, want o1", got)
	}
	if cmd.GetOrderId() != "o1" {
		t.Errorf("order_id = %q, want o1 (from the path)", cmd.GetOrderId())
	}
	// The digest is what the signature COVERS. Dropping it would let the payload
	// change after the second signature was given.
	if cmd.GetDigest() != "sha256:beef" {
		t.Errorf("digest = %q, want sha256:beef", cmd.GetDigest())
	}
}

// THE CLIENT CANNOT NAME THE APPROVER OR THE ORDER. The path and the principal win;
// a body that says otherwise is a caller signing as somebody else, for something
// else — the forged-actor defect #444 fixed on the override path, arriving here.
func TestApproveOverridesAForgedBody(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/approve", strings.NewReader(
		`{"digest":"sha256:beef","orderId":"o-other","metadata":{"issuer":"user:attacker","targetId":"o-other"}}`))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(req, "carol", approverRole))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	cmd := pub.last.Payload.(*orderpb.ApproveOrder)
	if cmd.GetOrderId() != "o1" || cmd.GetMetadata().GetTargetId() != "o1" {
		t.Errorf("order/target = %q/%q, want o1 — the path is the authority, not the body",
			cmd.GetOrderId(), cmd.GetMetadata().GetTargetId())
	}
	if got := cmd.GetMetadata().GetIssuer(); got != "user:carol" {
		t.Errorf("issuer = %q, want user:carol — a forged issuer must be overridden", got)
	}
}

// AN APPROVAL WITH NO DIGEST SIGNS NOTHING, and it must not reach the bus looking
// like an approval that does. The OMS remains the authority on whether the digest
// MATCHES what it stored — this is the shape check, not the signature check, and it
// is refused here only because a 202 followed by a silent bus-side rejection is the
// failure mode this repository refuses to ship.
func TestApproveRefusesAnEmptyDigest(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/approve", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(req, "carol", approverRole))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if pub.last != nil {
		t.Fatal("a digest-less approval was published")
	}
}

// WITH NO APPROVER NAMED, THE ROUTE IS ABSENT — 404, NOT 403 (#535).
//
// authz.Approve is carried by no role unless API_GATEWAY_APPROVE_ROLE names one. A
// registered route whose capability nobody holds refuses EVERY principal that
// exists, and "you may not" is a false answer when the truth is "nobody may, in this
// deployment". That is indistinguishable from a control working as designed, which
// is how authz.Fund survived from #415 to #535.
func TestApproveIsNotRegisteredWithoutAnApprover(t *testing.T) {
	pub := &fakePub{}
	m := approveMux()
	New(pub, "", halt.OpenGate(nil)).Routes(m) // no API_GATEWAY_APPROVE_ROLE

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(approveRequest(), "carol", approverRole))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("the approve route answered %d with no approver configured, want 404.\n"+
			"403 sends the operator looking for a role no deployment could grant them; 404 says there "+
			"is no approval surface here, which is true and actionable.", rr.Code)
	}
}

// AN APPROVER CANNOT TRADE — the reverse direction, and the half that decides
// whether Approve is a distinct authority or a subset of Trade. A signatory who can
// also originate the order they release has bought the platform a name and no
// separation.
func TestAnApproveTokenCannotSubmitOrCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{
		{"submit", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(
				`{"portfolioId":"pf1","instrumentId":"AAPL","side":"SIDE_BUY","quantity":{"coefficient":"100","exponent":0},"orderType":"ORDER_TYPE_MARKET","timeInForce":"TIME_IN_FORCE_DAY"}`))
		}},
		{"cancel", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/orders/o1/cancel", strings.NewReader(`{}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakePub{}
			m := approveMux()
			New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

			rr := httptest.NewRecorder()
			m.ServeHTTP(rr, asRole(tc.req(), "carol", approverRole))

			if rr.Code != http.StatusForbidden {
				t.Fatalf("an APPROVE token %sed an order: status = %d, want 403", tc.name, rr.Code)
			}
			if pub.last != nil {
				t.Fatalf("an approver published %q on the capital path", pub.last.EventType)
			}
		})
	}
}
