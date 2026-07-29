package binance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/coder/websocket"
)

type fakeStream struct {
	frames [][]byte
	i      int
}

func (f *fakeStream) Recv(context.Context) ([]byte, error) {
	if f.i >= len(f.frames) {
		return nil, io.EOF
	}
	b := f.frames[f.i]
	f.i++
	return b, nil
}

type fakeLookup map[string]*orderpb.OrderState

func (l fakeLookup) Lookup(id string) (*orderpb.OrderState, bool) { s, ok := l[id]; return s, ok }

func kanzOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: bdec("1"),
	}
}

func ingesterOver(frames [][]byte, cap *reconCapture) *UserDataIngester {
	return newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: frames},
		Lookup: fakeLookup{"o1": kanzOrder("o1")},
		Pub:    cap, Venue: "BINANCE", Tenant: "fund-alpha",
	})
}

func TestUserData_FilledReportBecomesOrderFilled(t *testing.T) {
	cap := &reconCapture{}
	report := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`
	_ = ingesterOver([][]byte{[]byte(report)}, cap).Run(context.Background()) // returns io.EOF at end

	var ev *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			ev = f
		}
	}
	if ev == nil {
		t.Fatal("no OrderFilled emitted from the fill report")
	}
	if ev.GetFill().GetFillId() != "BTCUSDT-7" {
		t.Fatalf("fill_id = %q, want BTCUSDT-7 (deterministic ⇒ dedups the sync path)", ev.GetFill().GetFillId())
	}
	if ev.GetState().GetPortfolioId() != "fund-alpha" || ev.GetState().GetInstrumentId() != "BTC-USD" {
		t.Fatal("fill not enriched with Kanz order context")
	}
	if ev.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED", ev.GetState().GetStatus())
	}
}

func TestUserData_PartialAndNoiseHandling(t *testing.T) {
	cap := &reconCapture{}
	frames := [][]byte{
		[]byte(`{"e":"executionReport","s":"BTCUSDT","c":"o1","x":"NEW","X":"NEW"}`),                                         // not a fill
		[]byte(`{"e":"outboundAccountPosition"}`),                                                                            // unrelated event
		[]byte(`{"e":"executionReport","s":"BTCUSDT","c":"unknown","x":"TRADE","X":"FILLED","l":"1","L":"1","z":"1","t":9}`), // unknown order
		[]byte(`{"e":"executionReport","s":"BTCUSDT","c":"o1","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.5","L":"50000","z":"0.5","q":"1","t":8}`),
	}
	_ = ingesterOver(frames, cap).Run(context.Background())

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
}

// TestUserData_WebsocketTransport certifies the real coder/websocket transport
// hermetically: an httptest server serves the listenKey REST + the ws upgrade
// and pushes a frame; binanceUserDataWS connects and Recv reads it.
func TestUserData_WebsocketTransport(t *testing.T) {
	frame := `{"e":"executionReport","c":"o1","x":"TRADE","X":"FILLED"}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/userDataStream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"listenKey":"abc123"}`))
	})
	mux.HandleFunc("/ws/abc123", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		_ = c.Write(r.Context(), websocket.MessageText, []byte(frame))
		time.Sleep(100 * time.Millisecond)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ws := newBinanceUserDataWS(srv.URL, srv.URL, "key", nil)
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
