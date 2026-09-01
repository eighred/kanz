package okx

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/pkg/bus"
)

// --- okx-tagged test seams (mirror the binance-tagged helpers) ---

type okxCapture struct{ events []bus.Event }

func (c *okxCapture) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

type okxStream struct {
	frames [][]byte
	i      int
}

func (s *okxStream) Recv(context.Context) ([]byte, error) {
	if s.i >= len(s.frames) {
		return nil, io.EOF
	}
	b := s.frames[s.i]
	s.i++
	return b, nil
}

// okxOrders is the adapter's REAL order view — orderview.Memory behind
// orderview.Seam — and not a stub map, deliberately. #904 is a claim about what
// the view DOES with a fill, and a hand-written double would be free to answer
// however the test wanted.
type okxOrders struct {
	*orderview.Seam
	store *orderview.Memory
	errs  []error
}

func newOKXOrders(ids ...string) *okxOrders {
	f := &okxOrders{store: orderview.NewMemory()}
	f.Seam = orderview.NewSeam(f.store, func(err error) { f.errs = append(f.errs, err) })
	for _, id := range ids {
		if err := f.store.Record(context.Background(), okxKanzOrder(id)); err != nil {
			panic(err)
		}
	}
	return f
}

func (f *okxOrders) get(t *testing.T, id string) *orderpb.OrderState {
	t.Helper()
	st, _, ok, err := f.store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("order %s is not in the adapter's view (ok=%v err=%v)", id, ok, err)
	}
	return st
}

func (f *okxOrders) openIDs(t *testing.T) []string {
	t.Helper()
	open, err := f.store.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var ids []string
	for _, o := range open {
		ids = append(ids, o.GetOrderId())
	}
	return ids
}

func okxKanzOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: odec("1"),
	}
}

func okxIngesterOver(frames [][]byte, cap *okxCapture) *OKXUserDataIngester {
	return newOKXUserDataIngester(&okxStream{frames: frames}, newOKXOrders("o1"), cap, "OKX", "fund-alpha")
}

func TestOKXUserData_FillBecomesOrderFilled(t *testing.T) {
	cap := &okxCapture{}
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7","fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`
	_ = okxIngesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background())

	var ev *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			ev = f
		}
	}
	if ev == nil {
		t.Fatal("no OrderFilled emitted from the OKX fill push")
	}
	if ev.GetFill().GetFillId() != "BTC-USDT-7" {
		t.Fatalf("fill_id = %q, want BTC-USDT-7 (deterministic ⇒ dedups the sync path)", ev.GetFill().GetFillId())
	}
	if ev.GetState().GetPortfolioId() != "fund-alpha" || ev.GetState().GetInstrumentId() != "BTC-USD" {
		t.Fatal("fill not enriched with Kanz order context")
	}
	if ev.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED", ev.GetState().GetStatus())
	}
	if got := dec.FromProto(ev.GetFill().GetFee().GetAmount()); got.Cmp(big.NewRat(1, 20)) != 0 { // 0.05 magnitude
		t.Fatalf("fee = %s, want 0.05", got.RatString())
	}
}

func TestOKXUserData_PartialAndNoiseHandling(t *testing.T) {
	cap := &okxCapture{}
	frames := [][]byte{
		[]byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"live","fillSz":"0"}]}`),                      // state change, not a fill
		[]byte(`{"arg":{"channel":"account"},"data":[{"ccy":"USDT"}]}`),                                                                       // unrelated channel
		[]byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"unknown","state":"filled","fillSz":"1","tradeId":"9"}]}`), // unknown order
		[]byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"partially_filled","fillSz":"0.5","fillPx":"50000","accFillSz":"0.5","tradeId":"8","uTime":"1700000000000"}]}`),
	}
	_ = okxIngesterOver(frames, cap).Run(context.Background())

	if len(cap.events) != 1 {
		t.Fatalf("emitted %d FACTs, want 1 (only the known partial fill)", len(cap.events))
	}
	pf, ok := cap.events[0].Payload.(*orderpb.OrderPartiallyFilled)
	if !ok {
		t.Fatalf("want OrderPartiallyFilled, got %T", cap.events[0].Payload)
	}
	if pf.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatal("partial fill status wrong")
	}
	if pf.GetFill().GetFillId() != "BTC-USDT-8" {
		t.Fatalf("fill_id = %q, want BTC-USDT-8", pf.GetFill().GetFillId())
	}
}

// TestOKXUserData_WebsocketTransport certifies the real coder/websocket
// transport hermetically: an httptest server serves the OKX v5 private ws
// handshake (login ack + subscribe) and pushes an orders frame; okxUserDataWS
// logs in, subscribes, and Recv reads the frame.
func TestOKXUserData_WebsocketTransport(t *testing.T) {
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"filled","fillSz":"1"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/v5/private", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		// login frame → ack
		if _, login, err := c.Read(ctx); err != nil || !containsOp(login, "login") {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"event":"login","code":"0"}`))
		// subscribe frame → data push
		if _, sub, err := c.Read(ctx); err != nil || !containsOp(sub, "subscribe") {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(frame))
		time.Sleep(100 * time.Millisecond)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/v5/private"
	ws := newOKXUserDataWS(wsURL, "key", "secret", "pass")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer stop()

	got, err := ws.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if string(got) != frame {
		t.Fatalf("frame = %q, want %q", got, frame)
	}
}

func containsOp(raw []byte, op string) bool {
	var m struct {
		Op string `json:"op"`
	}
	return json.Unmarshal(raw, &m) == nil && m.Op == op
}

// A GARBLED PRICE MUST NOT PUBLISH A FILL AT ZERO (#94).
//
// ParseDec answered anything big.Rat could not read with the ZERO Decimal, so a
// fillPx of "" or "null" — an OKX field the platform does not control — became an
// order.order.filled FACT carrying price 0. The ledger folds that, and a zero
// execution price does not look wrong anywhere downstream: it looks free.
//
// The correct outcome is a refusal. The message is nacked, the reconciler
// re-reads venue truth from REST, and nothing fabricated reaches the book.
func TestOKXUserData_AGarbledFillNumberIsRefusedNotZeroed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"unparseable price", `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"","accFillSz":"1","tradeId":"7","uTime":"1700000000000"}]}`},
		{"garbage price", `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"null","accFillSz":"1","tradeId":"7","uTime":"1700000000000"}]}`},
		{"garbage accFillSz", `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"n/a","tradeId":"7","uTime":"1700000000000"}]}`},
		{"garbage fee", `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7","fillFee":"??","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &okxCapture{}
			_ = okxIngesterOver([][]byte{[]byte(tc.frame)}, cap).Run(context.Background())

			for _, e := range cap.events {
				switch p := e.Payload.(type) {
				case *orderpb.OrderFilled:
					t.Fatalf("published a fill built from an unreadable number: qty=%s price=%s — "+
						"a zero here reads as a free execution, not as an error",
						dec.Str(dec.FromProto(p.GetFill().GetQuantity())),
						dec.Str(dec.FromProto(p.GetFill().GetPrice())))
				case *orderpb.OrderPartiallyFilled:
					t.Fatalf("published a partial fill built from an unreadable number: price=%s",
						dec.Str(dec.FromProto(p.GetFill().GetPrice())))
				}
			}
		})
	}
}

// A LARGE FILL PUBLISHES ITS ACTUAL SIZE (#94).
//
// ParseDec wrapped above ~92.2 billion units at scale 8, so a trillion-unit fill
// on a token OKX lists published as 77662796314.5224192 — the ledger would book
// about 7.8% of a real execution and the position book would agree with it.
func TestOKXUserData_ALargeFillIsNotWrapped(t *testing.T) {
	const qty = "1000000000000" // 1e12 units — an ordinary meme-coin fill

	cap := &okxCapture{}
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"filled","fillSz":"` +
		qty + `","fillPx":"0.00001","accFillSz":"` + qty + `","tradeId":"7","uTime":"1700000000000"}]}`
	_ = okxIngesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background())

	var ev *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			ev = f
		}
	}
	if ev == nil {
		t.Fatal("no OrderFilled emitted — a large but representable fill must still publish")
	}
	if got := dec.Str(dec.FromProto(ev.GetFill().GetQuantity())); got != qty {
		t.Fatalf("published fill quantity = %s, want %s — the ledger would book a size nobody traded", got, qty)
	}
	if got := dec.Str(dec.FromProto(ev.GetState().GetFilledQuantity())); got != qty {
		t.Fatalf("healed filled quantity = %s, want %s", got, qty)
	}
}

// A TRIGGERED STOP'S FILL IS ATTRIBUTED BY algoClOrdId (#485).
//
// THIS IS THE ASSERTION THE WHOLE FEATURE RESTS ON. A conditional order is not
// the order that fills — it creates one when it fires, and OKX gives that order
// a clOrdId OF ITS OWN. Observed on the demo API on 2026-08-15 by firing a real
// trigger and reading the order back:
//
//	ordId        3833848820050333696
//	clOrdId      O3833848819892766720   <- OKX's, not ours
//	algoClOrdId  kanzfire1786759936     <- ours
//
// Read clOrdId here and the lookup misses, no FACT is published, and a REAL
// EXECUTION GOES UNBOOKED while the venue reports success. That is the worst
// outcome this connector can produce: the money moved and the books do not know.
func TestUserData_ATriggeredStopsFillIsAttributedByAlgoClOrdId(t *testing.T) {
	cap := &okxCapture{}
	// Exactly the shape OKX pushes for a triggered stop: OKX's own generated
	// clOrdId, and ours on algoClOrdId.
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"3833848820050333696",` +
		`"clOrdId":"O3833848819892766720","algoClOrdId":"o1","state":"filled","fillSz":"1",` +
		`"fillPx":"50000","accFillSz":"1","tradeId":"7","fillFee":"-0.05","fillFeeCcy":"USDT",` +
		`"uTime":"1700000000000"}]}`
	_ = okxIngesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background())

	var ev *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			ev = f
		}
	}
	if ev == nil {
		t.Fatal("a triggered stop's fill produced NO OrderFilled — the lookup missed, so a real " +
			"execution is unbooked while the venue reports success")
	}
	if ev.GetOrderId() != "o1" {
		t.Fatalf("OrderFilled names order %q, want o1 — OKX's own clOrdId was used as though it "+
			"were ours, so the fill is attributed to an order that does not exist",
			ev.GetOrderId())
	}
}

// AND AN ORDINARY ORDER IS STILL ATTRIBUTED BY clOrdId. Preferring algoClOrdId
// must not break the path every non-stop order takes — it is absent on those,
// and an empty preference would attribute every ordinary fill to "".
func TestUserData_AnOrdinaryFillIsStillAttributedByClOrdId(t *testing.T) {
	cap := &okxCapture{}
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
		`"algoClOrdId":"","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1",` +
		`"tradeId":"7","fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`
	_ = okxIngesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background())

	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok && f.GetOrderId() == "o1" {
			return
		}
	}
	t.Fatal("an ordinary fill was not attributed by clOrdId — preferring algoClOrdId broke the " +
		"path every non-stop order takes")
}
