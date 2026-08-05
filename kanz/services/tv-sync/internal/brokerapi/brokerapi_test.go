package brokerapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/tv-sync/internal/projection"

	"github.com/eighred/kanz/pkg/auth"
)

func dec(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }

// foldFill folds a fully-filled order into the projection for a tenant/account.
func foldFill(t *testing.T, p *projection.Projection, tenant, account, orderID, inst string, side orderpb.Side, qty, price int64) {
	t.Helper()
	st := &orderpb.OrderState{
		OrderId: orderID, PortfolioId: account, InstrumentId: inst, Side: side,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: dec(qty),
		FilledQuantity: dec(qty), LeavesQuantity: dec(0), AverageFillPrice: dec(price),
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, AsOf: timestamppb.Now(),
	}
	ev := &orderpb.OrderFilled{OrderId: orderID, Fill: &orderpb.Fill{
		FillId: "fill-" + orderID, OrderId: orderID, InstrumentId: inst, Side: side,
		Quantity: dec(qty), Price: dec(price), Venue: "XNAS", ExecutedAt: timestamppb.Now(),
	}, State: st}
	b, err := proto.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Handle(context.Background(), &envelopepb.Envelope{TenantId: tenant, EventType: "order.order.filled"}, b); err != nil {
		t.Fatal(err)
	}
}

func newHandler(t *testing.T) (*http.ServeMux, *projection.Projection) {
	t.Helper()
	p := projection.New(time.Now, nil)
	mux := http.NewServeMux()
	New(p).Routes(mux)
	return mux, p
}

func get(t *testing.T, mux *http.ServeMux, path, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tenant != "" {
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestREST_AccountsPositionsState(t *testing.T) {
	mux, p := newHandler(t)
	foldFill(t, p, "acme", "fund-alpha", "o1", "BTC", orderpb.Side_SIDE_BUY, 2, 100)
	foldFill(t, p, "acme", "fund-alpha", "o2", "BTC", orderpb.Side_SIDE_SELL, 1, 150)

	if rec := get(t, mux, "/broker/accounts", "acme"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "fund-alpha") {
		t.Fatalf("accounts = %d %s", rec.Code, rec.Body)
	}

	rec := get(t, mux, "/broker/accounts/fund-alpha/positions", "acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("positions = %d", rec.Code)
	}
	var pos struct {
		Positions []projection.PositionDTO `json:"positions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pos)
	if len(pos.Positions) != 1 || pos.Positions[0].Qty != "1" || pos.Positions[0].RealizedPnl != "50" {
		t.Fatalf("positions = %+v", pos.Positions)
	}

	rec = get(t, mux, "/broker/accounts/fund-alpha/state", "acme")
	var st projection.StateDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.RealizedPnl != "50" || st.OpenPositions != 1 {
		t.Fatalf("state = %+v", st)
	}

	rec = get(t, mux, "/broker/accounts/fund-alpha/executions", "acme")
	if !strings.Contains(rec.Body.String(), "fill-o1") {
		t.Fatalf("executions = %s", rec.Body)
	}
}

func TestREST_MissingTenantUnauthorized(t *testing.T) {
	mux, p := newHandler(t)
	foldFill(t, p, "acme", "fund-alpha", "o1", "BTC", orderpb.Side_SIDE_BUY, 1, 100)
	if rec := get(t, mux, "/broker/accounts/fund-alpha/positions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant = %d, want 401", rec.Code)
	}
}

func TestREST_CrossTenantIsNotFound(t *testing.T) {
	mux, p := newHandler(t)
	foldFill(t, p, "acme", "fund-alpha", "o1", "BTC", orderpb.Side_SIDE_BUY, 1, 100)
	// globex asking for acme's account: not found, never leaked.
	if rec := get(t, mux, "/broker/accounts/fund-alpha/positions", "globex"); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant = %d, want 404", rec.Code)
	}
	if rec := get(t, mux, "/broker/accounts", "globex"); strings.Contains(rec.Body.String(), "fund-alpha") {
		t.Fatal("globex saw acme's account in the account list")
	}
}

func TestREST_AsOfBitemporal(t *testing.T) {
	mux, p := newHandler(t)
	foldFill(t, p, "acme", "fund-alpha", "o1", "BTC", orderpb.Side_SIDE_BUY, 1, 100)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	// As-of an hour ago: nothing was known, so positions are empty.
	rec := get(t, mux, "/broker/accounts/fund-alpha/positions?as_of="+past, "acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("as_of = %d", rec.Code)
	}
	var pos struct {
		Positions []projection.PositionDTO `json:"positions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pos)
	if len(pos.Positions) != 0 {
		t.Fatalf("as-of-past positions = %+v, want empty", pos.Positions)
	}
	// Bad as_of → 400.
	if rec := get(t, mux, "/broker/accounts/fund-alpha/positions?as_of=notatime", "acme"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad as_of = %d, want 400", rec.Code)
	}
}

func TestStream_DeliversDeltas(t *testing.T) {
	p := projection.New(time.Now, nil)
	foldFill(t, p, "acme", "fund-alpha", "seed", "BTC", orderpb.Side_SIDE_BUY, 1, 100) // account must exist
	mux := http.NewServeMux()
	New(p).Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/broker/accounts/fund-alpha/stream", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream open = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// Fold a new fill; the SSE client should receive an event.
	go func() {
		time.Sleep(100 * time.Millisecond)
		foldFill(t, p, "acme", "fund-alpha", "o1", "BTC", orderpb.Side_SIDE_BUY, 1, 120)
	}()

	sc := bufio.NewScanner(resp.Body)
	done := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event:") {
				done <- line
				return
			}
		}
	}()
	select {
	case line := <-done:
		if !strings.Contains(line, "execution") && !strings.Contains(line, "order") &&
			!strings.Contains(line, "position") && !strings.Contains(line, "state") {
			t.Fatalf("unexpected event line: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE event received")
	}
}

// TestStream_SurvivesTheServerWriteTimeout runs the REAL handler behind a server
// carrying a WriteTimeout, which is what tv-sync now listens on (#235).
//
// TestStream_DeliversDeltas above uses httptest.NewServer, whose Config sets no
// timeouts — so it proves the stream works on a server this service does not run.
// internal/platform/httpserver gives every server in the estate a WriteTimeout, and
// WriteTimeout bounds the WHOLE response: without the SetWriteDeadline lift in
// stream(), this endpoint is severed mid-session with no status, no body and nothing
// logged, and the Trading Terminal shows a book that has quietly stopped updating.
//
// Nothing else catches that. The arch guard checks that stream() CALLS
// SetWriteDeadline; the httpserver tests check the CALL WORKS in isolation. Only
// this one runs the actual endpoint against the actual bound.
//
// The timeout is milliseconds rather than the production two minutes so the test is
// fast — the mechanism is under test, not the number.
func TestStream_SurvivesTheServerWriteTimeout(t *testing.T) {
	const writeTimeout = 250 * time.Millisecond

	p := projection.New(time.Now, nil)
	foldFill(t, p, "acme", "fund-alpha", "seed", "BTC", orderpb.Side_SIDE_BUY, 1, 100)
	mux := http.NewServeMux()
	New(p).Routes(mux)

	srv := httptest.NewUnstartedServer(mux)
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/broker/accounts/fund-alpha/stream", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream open = %d, want 200 (the handler refuses rather than opening a "+
			"stream it cannot keep alive — see the SetWriteDeadline branch)", resp.StatusCode)
	}

	// Fold WELL AFTER the WriteTimeout would have fired. An unlifted deadline
	// severs the connection at 250ms, so this delta could never arrive.
	go func() {
		time.Sleep(3 * writeTimeout)
		foldFill(t, p, "acme", "fund-alpha", "o-late", "BTC", orderpb.Side_SIDE_BUY, 1, 120)
	}()

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event:") {
				got <- line
				return
			}
		}
		got <- "" // stream ended without an event: severed
	}()

	select {
	case line := <-got:
		if line == "" {
			t.Fatalf("the stream closed before a delta folded %v after it opened, with a "+
				"%v WriteTimeout on the server.\n\n"+
				"That is the endpoint being severed by the server's own write bound. The fix "+
				"is the http.NewResponseController(w).SetWriteDeadline(time.Time{}) call in "+
				"stream() — NOT removing WriteTimeout from tv-sync's server, which would "+
				"restore the unbounded connection of #235 for every ordinary /broker read "+
				"this service also serves.", 3*writeTimeout, writeTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE event received")
	}
}

func TestConfig(t *testing.T) {
	mux, _ := newHandler(t)
	rec := get(t, mux, "/broker/config", "acme")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "supportsPositions") {
		t.Fatalf("config = %d %s", rec.Code, rec.Body)
	}
}
