package main

// THE ORDER THIS HARNESS PUTS ON THE WIRE (#245's rule applied to the write side).
//
// #245's stance about test/load/ingest is the one that governs here: "a load tool
// that cannot publish measures nothing". A generator whose orders the platform
// refuses does not report a failed load test — it reports a load test that found
// no problems, because no load ever arrived. The write side has a sharper version
// of the same trap: an order the PRE-TRADE GATE refuses still gets a 202 from the
// gateway, so a harness sending unvaluable orders would report clean throughput
// for the refusal path at every rate.

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/orderid"
)

func testSubmitter(t *testing.T, cfg config) *submitter {
	t.Helper()
	s, err := newSubmitter(cfg)
	if err != nil {
		t.Fatalf("newSubmitter: %v", err)
	}
	return s
}

// THE ORDER ID IS THE SHAPE THE ESTATE ACCEPTS. A hyphenated UUID is 36
// characters and OKX refuses a clOrdId over 32, which made every gateway-minted
// order unplaceable there (#723's neighbour). The id is also stamped straight
// into the venue's client order id, so a load harness minting its own must mint
// the same shape.
func TestTheMintedOrderIDIsOneTheEstateAccepts(t *testing.T) {
	s := testSubmitter(t, config{})
	for i := 0; i < 100; i++ {
		id := s.mintID()
		if err := orderid.Valid(id); err != nil {
			t.Fatalf("mintID produced %q: %v", id, err)
		}
		if len(id) != orderid.MaxLen {
			t.Fatalf("mintID produced %d characters, want %d", len(id), orderid.MaxLen)
		}
		if !strings.HasPrefix(id, s.runTag) {
			t.Fatalf("%q does not carry this run's tag %q — the FACT stream is shared and retained, "+
				"so without the tag this harness cannot tell its own orders from an earlier run's",
				id, s.runTag)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("%q is not hex: %v", id, err)
		}
	}
}

// TWO RUNS DO NOT SHARE A TAG. If they did, a re-run against a broker that still
// retains the last one's FACTs would count those as its own — the "reused broker
// fails tests that count" trap, arriving through the harness rather than the
// stream.
func TestEachRunGetsItsOwnTag(t *testing.T) {
	a, b := testSubmitter(t, config{}), testSubmitter(t, config{})
	if a.runTag == b.runTag {
		t.Fatalf("two submitters minted the same run tag %q", a.runTag)
	}
}

// THE ORDER IS ONE THE PRE-TRADE GATE CAN VALUE, which is what makes the
// measurement about admission rather than about refusal. An order with no usable
// price is refused as Unpriced BEFORE the rule engine, the book projection or the
// margin resolution run — so a harness sending market orders at a rig with a
// quiet price spine would report the throughput of a control returning at its
// first branch.
func TestTheGeneratedOrderCarriesItsOwnPrice(t *testing.T) {
	s := testSubmitter(t, config{portfolio: "PF1", instrument: "AAPL"})
	o := s.order("abc123")

	if o.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT {
		t.Errorf("order_type = %v, want LIMIT — a MARKET order is valued from the mark fold, and an "+
			"unpriced order is refused before the gate does any work", o.GetOrderType())
	}
	if p := o.GetLimitPrice(); p == nil || p.GetCoefficient() <= 0 {
		t.Errorf("limit_price = %v, want a positive price; the gate refuses a non-positive one as "+
			"Unpriced", p)
	}
	if q := o.GetQuantity(); q == nil || q.GetCoefficient() <= 0 {
		t.Errorf("quantity = %v, want strictly positive (direction is carried by side)", q)
	}
	if o.GetSide() == orderpb.Side_SIDE_UNSPECIFIED ||
		o.GetTimeInForce() == orderpb.TimeInForce_TIME_IN_FORCE_UNSPECIFIED {
		t.Error("side and time_in_force must both be set; UNSPECIFIED is refused at admission")
	}
	if o.GetPortfolioId() != "PF1" || o.GetInstrumentId() != "AAPL" {
		t.Errorf("the order does not carry the configured portfolio/instrument: %+v", o)
	}
}

// THE BODY IS WHAT THE GATEWAY PARSES. The handler unmarshals protojson into an
// order.v1.SubmitOrder, so a body this harness builds by hand would be a second
// spelling of the command that could drift; it builds the proto and marshals it.
func TestTheSubmittedBodyRoundTripsAsASubmitOrder(t *testing.T) {
	s := testSubmitter(t, config{portfolio: "PF1", instrument: "AAPL"})
	raw, err := protojson.Marshal(s.order("deadbeef"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back orderpb.SubmitOrder
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &back); err != nil {
		t.Fatalf("the gateway's own unmarshal options reject this body: %v", err)
	}
	if back.GetOrderId() != "deadbeef" {
		t.Errorf("order_id did not survive the round trip: %q", back.GetOrderId())
	}
}

// EVERY SUBMISSION IS NAMEABLE ON RETRY. The gateway refuses a submit carrying
// neither an order_id nor an Idempotency-Key (#723), because a retry after a
// timeout would otherwise be indistinguishable from a second order and would
// place one. A load harness generating thousands of submissions is the last place
// that should be left to chance.
func TestEverySubmissionCarriesAnIdempotencyKeyEqualToItsOrderID(t *testing.T) {
	var gotKey, gotAuth string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		gotAuth = r.Header.Get("Authorization")
		body = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := testSubmitter(t, config{baseURL: srv.URL, token: "t0ken", portfolio: "PF1", instrument: "AAPL"})
	sub, code, err := s.submitOne(context.Background())
	if err != nil || code != http.StatusAccepted {
		t.Fatalf("submitOne: code=%d err=%v", code, err)
	}
	if gotKey != sub.id {
		t.Errorf("Idempotency-Key = %q, want the order id %q. A key that differs per attempt defeats "+
			"the broker's dedup window, the OMS's claim and the gateway's own idempotency at once",
			gotKey, sub.id)
	}
	if gotAuth != "Bearer t0ken" {
		t.Errorf("Authorization = %q — an unauthenticated run measures the latency of 401s", gotAuth)
	}
}

// THE FRONT DOOR IS THE PATH, and this asserts the URL rather than trusting the
// doc comment above submitter. test/arch/load_harness_front_door_test.go asserts
// the same property structurally over anything that lands in test/load.
func TestSubmissionsGoToTheGatewaysOrderRoute(t *testing.T) {
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := testSubmitter(t, config{baseURL: srv.URL + "/", token: "t"})
	if _, _, err := s.submitOne(context.Background()); err != nil {
		t.Fatalf("submitOne: %v", err)
	}
	if method != http.MethodPost || path != "/v1/orders" {
		t.Errorf("submitted %s %s, want POST /v1/orders", method, path)
	}
}

// A NON-202 IS NAMED, NOT FOLDED INTO AN ERROR RATE. 429 is the gateway shedding,
// 423 is the platform halted, 403 is an entitlement and 0 is this harness's own
// request failing — four different conclusions, and a single rate reports them as
// one number.
func TestNonAcceptedResponsesAreNamedIndividually(t *testing.T) {
	got := describeStatuses(map[int]int{0: 2, 403: 1, 429: 5})
	for _, want := range []string{"no response", "http 403 x1", "http 429 x5"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeStatuses = %q, missing %q", got, want)
		}
	}
}
