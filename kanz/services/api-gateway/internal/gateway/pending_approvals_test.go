package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// AN APPROVE ROUTE WITH NO WAY TO KNOW WHAT TO APPROVE (#539).
//
// #546 gave the estate POST /v1/orders/{id}/approve, and #547 made an unsigned
// proposal announce its own death. Between them they left the thing an approver
// actually needs missing: a way to SEE the queue.
//
// The consequences compound, and none of them has a workaround:
//
//   - the approve route refuses an empty digest with 400, so an approver needs
//     the order_id AND the digest before they can sign anything;
//   - a held order is deliberately absent from the orders table, so
//     GET /v1/portfolios/{id}/orders never shows it;
//   - the digest rides OrderPendingApproval on NATS, which an approver outside
//     the cluster cannot subscribe to.
//
// So the OMS served ListPendingApprovals and the only reference to it outside
// that service was a test double written to satisfy the interface. Act one got
// its queue route — GET /v1/exceptions/pending-overrides — and act three did
// not. That is #539's own defect, narrowed from "no route at all" to "no way to
// discover what the route is for".

// servable is a double that WOULD answer 200 if the route were reached. Using an
// empty fakeClient here would make every negative case pass for the wrong
// reason: a nil response has an empty owner tenant, so writeOwned answers 404
// from the TENANT gate and "the route is absent" is indistinguishable from "the
// route is present and refused me". Found by mutation — registering the route
// unconditionally did not fail these tests until this existed.
func servable() *fakeClient {
	return &fakeClient{pendingResp: &orderpb.ListPendingApprovalsResponse{OwnerTenant: testTenant}}
}

func approveMux(t *testing.T, fc *fakeClient, approveRole string) http.Handler {
	t.Helper()
	mux := authz.NewMux(authz.Grants{
		"analyst":    {authz.Read},
		"compliance": {authz.Read, authz.Approve},
	}, nil)
	gateway.New(fc, fc, fc, approveRole, nil).Routes(mux)
	return mux
}

func asRole(t *testing.T, mux http.Handler, role, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	p := &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{role}}
	mux.ServeHTTP(rr, req.WithContext(middleware.WithPrincipal(req.Context(), p)))
	return rr.Result()
}

// THE QUEUE IS READABLE, AND THE REQUEST REACHES THE OMS UNMANGLED.
func TestAnApproverCanSeeWhatIsWaitingOnThem(t *testing.T) {
	fc := &fakeClient{pendingResp: &orderpb.ListPendingApprovalsResponse{
		OwnerTenant: testTenant,
		Pending: []*orderpb.PendingApproval{{
			OrderId: "o-1", Proposer: "user:alice@kanz", Digest: "sha256:abc",
		}},
	}}
	mux := approveMux(t, fc, "compliance")

	res := asRole(t, mux, "compliance", "/v1/orders/pending-approvals?portfolio=PF1&limit=25")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an approver cannot see the queue they are supposed to act on", res.StatusCode)
	}

	if fc.gotPending == nil {
		t.Fatal("the OMS was never asked — the route answered from nothing, which would certify " +
			"an empty queue for a book full of held orders")
	}
	if fc.gotPending.GetPortfolioId() != "PF1" {
		t.Errorf("portfolio_id = %q, want PF1 — the filter was dropped", fc.gotPending.GetPortfolioId())
	}

	// THE DIGEST MUST SURVIVE THE TRANSCODING. Without it the approver cannot
	// call the approve route at all: it refuses an empty digest with 400.
	var body struct {
		Pending []struct {
			OrderID string `json:"order_id"`
			Digest  string `json:"digest"`
		} `json:"pending"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Pending) != 1 || body.Pending[0].Digest != "sha256:abc" {
		t.Fatalf("pending = %+v, want one entry carrying the digest — an approver who cannot read "+
			"the digest cannot sign, and the approve route rejects an empty one with 400", body.Pending)
	}
}

// A READ TOKEN CANNOT SEE THE QUEUE. It names the proposer of every unsigned
// change awaiting a second signature, and it is the working surface an approver
// acts from rather than a report.
func TestAReadTokenCannotSeeThePendingQueue(t *testing.T) {
	mux := approveMux(t, servable(), "compliance")

	if res := asRole(t, mux, "analyst", "/v1/orders/pending-approvals"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a READ token read the approval queue: status = %d, want 403", res.StatusCode)
	}
}

// WITH NO APPROVER NAMED THE ROUTE IS ABSENT — 404, NOT 403 (#535). Registered
// and granted to nobody is the third state: it refuses every principal that
// exists while reading as a working control.
func TestThePendingQueueIsNotRegisteredWithoutAnApprover(t *testing.T) {
	mux := approveMux(t, servable(), "")

	if res := asRole(t, mux, "compliance", "/v1/orders/pending-approvals"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d with no approver configured, want 404 — 403 sends an operator "+
			"looking for a role no deployment could grant them", res.StatusCode)
	}
}

// THE TENANT GATE STILL APPLIES. A queue whose owner_tenant is not the caller's
// is refused, and an OMS that was never given a tenant stamps an empty one and
// fails CLOSED — the same rule every other read on this handler follows.
func TestAnotherTenantsQueueIsRefused(t *testing.T) {
	fc := &fakeClient{pendingResp: &orderpb.ListPendingApprovalsResponse{OwnerTenant: "someone-else"}}
	mux := approveMux(t, fc, "compliance")

	if res := asRole(t, mux, "compliance", "/v1/orders/pending-approvals"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d for another tenant's queue, want 404", res.StatusCode)
	}
}
