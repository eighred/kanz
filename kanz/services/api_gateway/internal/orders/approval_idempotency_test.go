package orders

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
)

// TWO APPROVERS ARE TWO COMMANDS (#581).
//
// The idempotency key rides as the broker's Nats-Msg-Id and JetStream collapses
// duplicates inside the stream's window. Approve used the bare order id, so a
// second approver's signature looked to the broker like a retry of the first
// one's — and the first one is USUALLY REFUSED, because self-approval is the
// most common refusal in maker-checker: the proposer may not sign their own.
//
// Alice proposes and is refused; Bob signs; Bob's command is dropped by the
// broker; Bob is told 202; the OMS never sees it; the order stays pending with
// nothing saying why. That is #539's shape — an order that neither executes nor
// reports why — on the path built to end it.
//
// It became REACHABLE when #575 put the refusal on the pending queue. Until an
// approver could see the refusal, nobody knew to retry, so nothing exercised it.

// approveAs publishes one approval and returns the idempotency key it carried.
func approveAs(t *testing.T, orderID, subject, idemHeader string) string {
	t.Helper()
	pub := &fakePub{}
	m := approveMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders/"+orderID+"/approve",
		strings.NewReader(`{"digest":"sha256:beef"}`))
	if idemHeader != "" {
		req.Header.Set("Idempotency-Key", idemHeader)
	}
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, asRole(req, subject, approverRole))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("approve as %s: status = %d, want 202 (%s)", subject, rr.Code, rr.Body.String())
	}
	if pub.last == nil {
		t.Fatal("nothing was published, so there is no key to compare")
	}
	return pub.last.IdempotencyKey
}

func TestASecondApproverIsNotADuplicateOfTheFirst(t *testing.T) {
	alice := approveAs(t, "o1", "alice", "")
	bob := approveAs(t, "o1", "bob", "")

	if alice == bob {
		t.Fatalf("both approvals carry the same idempotency key %q.\n\n"+
			"That key becomes the broker's Nats-Msg-Id, so JetStream drops the second inside the "+
			"dedup window. Alice is refused for self-approval, Bob signs, Bob's command never "+
			"reaches the OMS, and Bob is told 202. The order stays pending and nobody is told why.",
			alice)
	}
}

// AND A SAME-SUBJECT RETRY MUST STILL COLLAPSE. This is the property the fix must
// not trade away: a double-clicked approve is one signature, not two, and the
// broker is what makes that true across pods.
func TestTheSameApproverRetryingIsStillOneCommand(t *testing.T) {
	first := approveAs(t, "o1", "bob", "")
	again := approveAs(t, "o1", "bob", "")

	if first != again {
		t.Fatalf("the same approver's retry produced two keys (%q, %q) — a double-click would "+
			"publish two approvals of one order", first, again)
	}
}

// THE SAME PERSON ON A DIFFERENT ORDER IS A DIFFERENT COMMAND TOO. A key derived
// from the subject alone would collapse every approval an approver ever makes.
func TestOneApproverOnTwoOrdersIsTwoCommands(t *testing.T) {
	one := approveAs(t, "o1", "bob", "")
	two := approveAs(t, "o2", "bob", "")

	if one == two {
		t.Fatalf("approving two different orders produced the same key %q — the second order's "+
			"approval would be dropped by the broker", one)
	}
}

// AN EXPLICIT HEADER STILL WINS. A client that sends Idempotency-Key has stated
// what it means by "the same request", and this gateway does not overrule it.
func TestAnExplicitIdempotencyKeyStillWins(t *testing.T) {
	if got := approveAs(t, "o1", "bob", "chosen-by-the-client"); got != "chosen-by-the-client" {
		t.Fatalf("idempotency key = %q, want the client's header — a caller that says two requests "+
			"are the same request is entitled to be believed", got)
	}
}

// SUBMIT IS UNCHANGED, and must stay so. An order is submitted once; a retry that
// lands on another pod must collapse, which is the property the manifest calls
// "the one thing that would have made scaling dangerous".
func TestSubmitStillKeysOnTheOrderIdAlone(t *testing.T) {
	pub := &fakePub{}
	m := testMux()
	New(pub, approverRole, halt.OpenGate(nil)).Routes(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/orders",
		strings.NewReader(`{"order_id":"o-sub","portfolio_id":"PF1","symbol":"BTC-USD","side":"BUY","quantity":{"units":"1"}}`))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, authed(req, "trader-1", "acme"))

	if pub.last == nil {
		t.Fatalf("submit published nothing: status = %d (%s)", rr.Code, rr.Body.String())
	}
	if pub.last.IdempotencyKey != "o-sub" {
		t.Fatalf("submit idempotency key = %q, want the bare order id. Submit is once-per-order "+
			"and its dedup is what stops a retry on another pod placing a second order.",
			pub.last.IdempotencyKey)
	}
}
