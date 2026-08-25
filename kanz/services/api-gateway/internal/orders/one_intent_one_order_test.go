package orders

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
)

// ONE CLIENT INTENT MUST BE AT MOST ONE ORDER, INCLUDING ACROSS A RETRY (#723).
//
// A submit is made at-most-once by exactly one value: Event.IdempotencyKey. It
// rides to the broker as Nats-Msg-Id (pkg/bus/producer.go), it is what
// JetStream collapses on, and it is what the OMS consumer claims
// (pkg/bus/consumer.go). Every layer below the gateway keys off it, so a key
// that differs between two attempts of ONE intent defeats all of them at once.
//
// The gateway can derive that key two ways, and both are honest:
//
//   - the client's Idempotency-Key header — the client has stated what it means
//     by "the same request", and the middleware claim covers it; or
//   - the client's order_id — a stable natural key, because a submit happens
//     once per order.
//
// With NEITHER, there is nothing stable to derive it from. `orderid.Mint()`
// produces a new id per attempt, so the retry of an ambiguous timeout published
// a second COMMAND with a second key, became a second order in the OMS, and —
// because internal/execution stamps order_id directly as the venue clOrdId —
// a second LIVE ORDER at the exchange. Three dedup layers, all missing, for one
// reason.
//
// Deriving the key from the request body was considered and rejected: two
// deliberately identical orders are a normal thing for a fund to send, and
// silently collapsing them is a worse failure than refusing an ambiguous one.
//
// So the unsafe combination is refused, which is CLAUDE.md's "fail loudly,
// never silently" — the client learns its order was NOT placed and is told the
// two ways to make it safe, instead of the platform quietly placing two.
const submitBodyNoID = `{"portfolioId":"pf1","instrumentId":"AAPL","side":"SIDE_BUY",` +
	`"quantity":{"coefficient":"100","exponent":0},"orderType":"ORDER_TYPE_MARKET",` +
	`"timeInForce":"TIME_IN_FORCE_DAY"}`

// submitIntent posts one attempt of a client intent. idemKey == "" sends no
// header at all, which is the case under test rather than an empty one.
func submitIntent(t *testing.T, pub *fakePub, body, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	h := New(pub, "", halt.OpenGate(nil))
	mux := testMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	req = authed(req, "alice", "acme")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// TestSubmit_NoOrderIDAndNoKey_IsRefusedNotMinted is the defect itself. Before
// #723 this answered 202 with a freshly minted order id, and did so again on
// every retry.
func TestSubmit_NoOrderIDAndNoKey_IsRefusedNotMinted(t *testing.T) {
	pub := &fakePub{}
	rr := submitIntent(t, pub, submitBodyNoID, "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a submit that cannot be made at-most-once must be "+
			"refused, not minted (body: %s)", rr.Code, rr.Body.String())
	}
	// THE REFUSAL MUST BE BEFORE THE PUBLISH, not a 400 written after the
	// command already reached the broker. A refused order that was placed anyway
	// is the defect wearing the fix's clothes.
	if pub.last != nil {
		t.Fatalf("a refused submit still published %q — the refusal must precede the publish",
			pub.last.IdempotencyKey)
	}
	// The client has to be able to ACT on this. Naming both remedies is the
	// difference between a refusal and an outage.
	body := rr.Body.String()
	for _, want := range []string{"Idempotency-Key", "order_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal does not name %q, so the client cannot tell what to do: %s", want, body)
		}
	}
}

// TestSubmit_MintingStaysLegalUnderAKey: the capability is not withdrawn. A
// client that names the operation still need not invent an order id — the key
// the broker dedups on is the header, so the mint is safe.
func TestSubmit_MintingStaysLegalUnderAKey(t *testing.T) {
	pub := &fakePub{}
	rr := submitIntent(t, pub, submitBodyNoID, "intent-1")

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
	}
	if pub.last == nil {
		t.Fatal("nothing published")
	}
	if pub.last.IdempotencyKey != "intent-1" {
		t.Fatalf("IdempotencyKey = %q, want the client's header — a minted id must never "+
			"become the dedup key", pub.last.IdempotencyKey)
	}
}

// TestSubmit_RetriedIntentCarriesOneKey is the property, stated directly: no
// accepted submit path lets ONE intent produce TWO keys. Both remedies are
// exercised, because a fix that only covers one leaves the other duplicating.
func TestSubmit_RetriedIntentCarriesOneKey(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		idemKey string
	}{
		{"client named the operation", submitBodyNoID, "intent-1"},
		{"client supplied the order id", `{"orderId":"o-1","portfolioId":"pf1"}`, ""},
		{"client did both", `{"orderId":"o-1","portfolioId":"pf1"}`, "intent-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Two separate handlers: an ambiguous timeout is retried against
			// whichever replica the load balancer picks, not the one that
			// happened to serve the first attempt.
			first := &fakePub{}
			second := &fakePub{}
			if rr := submitIntent(t, first, tc.body, tc.idemKey); rr.Code != http.StatusAccepted {
				t.Fatalf("first attempt = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
			}
			if rr := submitIntent(t, second, tc.body, tc.idemKey); rr.Code != http.StatusAccepted {
				t.Fatalf("retry = %d, want 202 (body: %s)", rr.Code, rr.Body.String())
			}
			if first.last.IdempotencyKey != second.last.IdempotencyKey {
				t.Fatalf("one intent produced two keys, %q then %q: the broker will not collapse "+
					"them and the venue gets two live orders",
					first.last.IdempotencyKey, second.last.IdempotencyKey)
			}
			if first.last.IdempotencyKey == "" {
				t.Fatal("empty IdempotencyKey — the envelope validator requires one for a COMMAND")
			}
		})
	}
}
