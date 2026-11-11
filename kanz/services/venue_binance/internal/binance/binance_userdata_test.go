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

	"github.com/eighred/kanz/internal/venueadapter/orderview"
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

// fakeOrders is the adapter's REAL order view — orderview.Memory behind
// orderview.Seam — and not a stub map, deliberately. #904 is a claim about what
// the view DOES with a fill, and a hand-written double would be free to answer
// however the test wanted; this exercises the same Progress merge, the same
// Terminal filter and the same eviction the connector runs in production.
type fakeOrders struct {
	*orderview.Seam
	store *orderview.Memory
	errs  []error
}

func newFakeOrders(ids ...string) *fakeOrders {
	f := &fakeOrders{store: orderview.NewMemory()}
	f.Seam = orderview.NewSeam(f.store, func(err error) { f.errs = append(f.errs, err) })
	for _, id := range ids {
		if err := f.store.Record(context.Background(), kanzOrder(id)); err != nil {
			panic(err)
		}
	}
	return f
}

// get is the adapter's own belief about one order, straight from the store.
func (f *fakeOrders) get(t *testing.T, id string) *orderpb.OrderState {
	t.Helper()
	st, _, ok, err := f.store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("order %s is not in the adapter's view (ok=%v err=%v)", id, ok, err)
	}
	return st
}

// openIDs is what the healing watchdog would reconcile this pass.
func (f *fakeOrders) openIDs(t *testing.T) []string {
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

func kanzOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: bdec("1"),
	}
}

func ingesterOver(frames [][]byte, cap *reconCapture) *UserDataIngester {
	return ingesterOverView(frames, cap, newFakeOrders("o1"))
}

func ingesterOverView(frames [][]byte, cap *reconCapture, view *fakeOrders) *UserDataIngester {
	return newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: frames},
		Orders: view,
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
