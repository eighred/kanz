package orders

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/platform/halt"
)

// POST /v1/orders IS THE PATH THE ISSUE MEASURED (#635): authz.go calls it the
// one that "reaches a live exchange", and an operator's kanz-halt did not touch
// it. These tests go through the REAL authz mux a client goes through — not the
// handler func — because a route that stops honouring the halt because it was
// re-registered somewhere else is the failure mode, not a hypothetical.

// haltedGate is a gate closed the way production closes it: by folding the
// lifecycle.v1.ModeChanged FACT cmd/kanz-halt publishes, through the same
// Gate.Handle the bus subscription calls.
func haltedGate(t *testing.T) *halt.Gate {
	t.Helper()
	g := halt.OpenGate(nil)
	payload, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:akif",
		Reason:    "risk breach on fund-alpha",
	})
	if err != nil {
		t.Fatalf("marshal ModeChanged: %v", err)
	}
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Gate.Handle: %v", err)
	}
	if !g.Halted() {
		t.Fatal("the operator's ModeChanged did not close the gate")
	}
	return g
}

const validOrderJSON = `{"portfolioId":"pf1","instrumentId":"AAPL","side":"SIDE_BUY",` +
	`"quantity":{"coefficient":"100","exponent":0},"orderType":"ORDER_TYPE_MARKET",` +
	`"timeInForce":"TIME_IN_FORCE_DAY"}`

func TestSubmit_RefusedWhileThePlatformIsHalted(t *testing.T) {
	pub := &fakePub{}
	h := New(pub, "", haltedGate(t))
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(validOrderJSON))
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423 Locked (body: %s)", rr.Code, rr.Body.String())
	}
	// THE ABSENCE OF THE SIDE EFFECT. A 423 with the COMMAND already on the bus
	// would be a halt in the response body only: the OMS would still receive it.
	if pub.last != nil {
		t.Fatalf("an order COMMAND was published during a declared halt: %s", pub.last.EventType)
	}
	// The operator's reason reaches the caller. A bare "locked" sends a trader to
	// find someone to ask, during the incident.
	if !strings.Contains(rr.Body.String(), "risk breach on fund-alpha") {
		t.Fatalf("body = %s, want it to carry the operator's reason", rr.Body.String())
	}
}

// DENY-BY-DEFAULT AT THE FRONT DOOR. A gateway that has not yet learned the
// platform mode refuses — the state every pod boots in, and the reason
// halt.Arm's failure path is survivable rather than fatal.
func TestSubmit_RefusedWhenTheGatewayHasNeverLearnedThePlatformMode(t *testing.T) {
	pub := &fakePub{}
	h := New(pub, "", halt.NewGate(nil))
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(validOrderJSON))
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423 (body: %s)", rr.Code, rr.Body.String())
	}
	if pub.last != nil {
		t.Fatalf("an order COMMAND was published by a gateway that has never learned the platform mode: %s",
			pub.last.EventType)
	}
}

// THE APPROVAL IS A NEW ORDER ARRIVING LATE. The OMS replays every gate when it
// releases a held proposal, so a signature collected before a halt must not be
// what puts an order in front of an exchange during one.
func TestApprove_RefusedWhileThePlatformIsHalted(t *testing.T) {
	pub := &fakePub{}
	mux := approveMux()
	New(pub, approverRole, haltedGate(t)).Routes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, asRole(approveRequest(), "carol", approverRole))

	if rr.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423 (body: %s)", rr.Code, rr.Body.String())
	}
	if pub.last != nil {
		t.Fatalf("an approval COMMAND was published during a declared halt: %s", pub.last.EventType)
	}
}

// A HALT DOES NOT LOCK THE EXITS (#635). Cancel is deliberately outside the
// gate: an operator who halts on a risk breach must still be able to withdraw
// what is resting, and a brake that jams the exits is worse than no brake.
//
// Asserted on the PUBLISHED COMMAND, not the status code alone — a 202 with
// nothing on the bus would be the same outage wearing a success.
func TestCancel_StillAcceptedWhileThePlatformIsHalted(t *testing.T) {
	pub := &fakePub{}
	h := New(pub, "", haltedGate(t))
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/cancel", strings.NewReader(``))
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — a halt must never stop a cancel (body: %s)", rr.Code, rr.Body.String())
	}
	if pub.last == nil || pub.last.EventType != subjectCancel {
		t.Fatal("no cancel COMMAND reached the bus during a halt — the exits are jammed")
	}
}

// A READ-ONLY GATEWAY ANSWERS ITS OWN TRUTH. With no publisher there is no write
// surface to brake, and "order writes are disabled" (503) must not be displaced
// by a fact about a path this deployment does not have.
func TestSubmit_ReadOnlyGatewayStillAnswers503NotHalted(t *testing.T) {
	h := New(nil, "", halt.NewGate(nil)) // no publisher, gate never opened
	mux := testMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(validOrderJSON))
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
}
