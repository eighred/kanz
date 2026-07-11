//go:build okx

package execution

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/coder/websocket"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/pkg/bus"
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

type okxLookup map[string]*orderpb.OrderState

func (l okxLookup) Lookup(id string) (*orderpb.OrderState, bool) { s, ok := l[id]; return s, ok }

func okxKanzOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: odec("1"),
	}
}

func okxIngesterOver(frames [][]byte, cap *okxCapture) *OKXUserDataIngester {
	ing := newOKXUserDataIngester(&okxStream{frames: frames}, okxLookup{"o1": okxKanzOrder("o1")}, cap, "OKX", "fund-alpha")
	return ing
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
