package trades

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// THE PROPERTY UNDER TEST IS THE ONE THE WHOLE INGESTION-COVERAGE RECORD RESTS
// ON (#591): a subscription proves it is alive WITHOUT any trade arriving.
//
// If liveness could only be reported off executions, coverage would be a
// function of trading activity — a quiet minute would report as unobserved, and
// the record would re-derive exactly the ambiguity it exists to resolve
// (bars/fold.go: "this process cannot tell 'nothing traded' from 'the feed was
// down'"). So these tests deliberately serve NO trades.
//
// Hermetic: the real coder/websocket transport against an httptest server. Trade
// streams are public market data, so there is no key and no network.

// drain keeps the server side reading. coder/websocket answers a ping and
// completes a close handshake only from inside a Read, so a test server that
// parks on ctx.Done() silently makes every ping time out — which would look
// exactly like the defect these tests exist to catch.
func drain(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

// recordingLiveness captures what a source attests.
type recordingLiveness struct {
	mu    sync.Mutex
	live  int
	downs []error
}

func (r *recordingLiveness) Live(time.Time) {
	r.mu.Lock()
	r.live++
	r.mu.Unlock()
}

func (r *recordingLiveness) Down(_ time.Time, err error) {
	r.mu.Lock()
	r.downs = append(r.downs, err)
	r.mu.Unlock()
}

func (r *recordingLiveness) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live, len(r.downs)
}

func (r *recordingLiveness) waitForLive(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := r.counts(); n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, _ := r.counts()
	t.Fatalf("only %d liveness observations after 3s, want %d — a subscription that cannot prove "+
		"itself alive while the market is quiet makes every quiet minute read as a hole", n, want)
}

// A BINANCE SUBSCRIPTION ATTESTS ITSELF OFF THE WEBSOCKET PING, WITH NO TRADES.
//
// Conn.Ping sends a ping and waits for the PONG, so a success is end-to-end
// evidence rather than a local write. That is the difference between "we had a
// socket object" and "the venue was still talking to us", and it is the only
// thing that can distinguish a half-open TCP connection — on which Read blocks
// forever and nothing errors — from a genuinely quiet market.
func TestBinanceAttestsLivenessWithoutAnyTrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		// Serve NOTHING. The server's only job is to answer PINGS — which
		// coder/websocket does automatically, but only from inside a Read call,
		// so the drain loop below is what makes the pong come back at all.
		drain(r.Context(), conn)
	}))
	defer srv.Close()

	obs := &recordingLiveness{}
	src := NewBinanceSource(BinanceConfig{
		Symbol:            "BTCUSDT",
		WSBase:            "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient:        srv.Client(),
		Liveness:          obs,
		HeartbeatInterval: 20 * time.Millisecond,
	})
	defer func() { _ = src.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Recv blocks forever on a silent stream, which is the situation being
	// tested: the liveness must come from the heartbeat, not from this call.
	go func() { _, _ = src.Recv(ctx) }()

	obs.waitForLive(t, 3)
	if _, downs := obs.counts(); downs != 0 {
		t.Errorf("a healthy silent stream reported %d break(s) — silence is not a fault", downs)
	}
}

// A NON-TRADE FRAME IS LIVENESS TOO, and it is reported BEFORE the "is this a
// trade" filter. Reporting after would tie coverage to trading activity.
func TestBinanceAttestsLivenessOnAControlFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		// A frame that is NOT an execution: the Recv loop skips it as data and
		// must still count it as evidence.
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"e":"ping","E":1}`))
		drain(r.Context(), conn)
	}))
	defer srv.Close()

	obs := &recordingLiveness{}
	src := NewBinanceSource(BinanceConfig{
		Symbol:            "BTCUSDT",
		WSBase:            "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient:        srv.Client(),
		Liveness:          obs,
		HeartbeatInterval: time.Hour, // the heartbeat must not be what proves this
	})
	defer func() { _ = src.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = src.Recv(ctx) }()

	obs.waitForLive(t, 1)
}

// A DEAD STREAM ATTESTS A BREAK, and a break is a different fact from silence:
// a fault that was actually SEEN, rather than the absence of evidence.
func TestBinanceAttestsABreakWhenTheStreamDies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Hang up immediately.
		_ = conn.CloseNow()
	}))
	defer srv.Close()

	obs := &recordingLiveness{}
	src := NewBinanceSource(BinanceConfig{
		Symbol:            "BTCUSDT",
		WSBase:            "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient:        srv.Client(),
		Liveness:          obs,
		HeartbeatInterval: time.Hour,
	})
	defer func() { _ = src.Close() }()

	if _, err := src.Recv(context.Background()); err == nil {
		t.Fatal("Recv returned no error on a hung-up stream")
	}
	if _, downs := obs.counts(); downs != 1 {
		t.Fatalf("attested %d breaks, want 1 — a fault nobody records is a hole in the record that "+
			"reads exactly like a quiet market", downs)
	}
}

// AN OKX SUBSCRIPTION ATTESTS ITSELF OFF THE KEEPALIVE PONG, WITH NO TRADES.
//
// OKX's keepalive is application-level: the client writes "ping" and the venue
// answers "pong". The WRITE succeeding proves only that the local send buffer
// accepted it — the pong is the evidence, and it arrives on the Recv loop, which
// is why liveness is reported there and not in keepAlive.
func TestOKXAttestsLivenessWithoutAnyTrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if string(data) == "ping" {
				if err := conn.Write(ctx, websocket.MessageText, []byte("pong")); err != nil {
					return
				}
				continue
			}
			// The subscribe. Ack it and then serve NOTHING.
			ack := `{"event":"subscribe","arg":{"channel":"trades","instId":"BTC-USDT"}}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(ack)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	obs := &recordingLiveness{}
	src := NewOKXSource(OKXConfig{
		InstID:            "BTC-USDT",
		WSURL:             "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient:        srv.Client(),
		Liveness:          obs,
		HeartbeatInterval: 20 * time.Millisecond,
	})
	defer func() { _ = src.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = src.Recv(ctx) }()

	obs.waitForLive(t, 3)
	if _, downs := obs.counts(); downs != 0 {
		t.Errorf("a healthy silent stream reported %d break(s)", downs)
	}
}

// A REJECTED SUBSCRIBE IS A BREAK, EVEN THOUGH THE SOCKET IS ANSWERING.
//
// This is the case an "is the connection up" check gets wrong. The frame arrived
// — the transport is fine — but the feed is delivering nothing, and attesting
// coverage for it would vouch for a subscription the venue refused.
func TestOKXAttestsABreakWhenTheSubscribeIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		_ = conn.Write(ctx, websocket.MessageText,
			[]byte(`{"event":"error","code":"60012","msg":"invalid request"}`))
		drain(ctx, conn)
	}))
	defer srv.Close()

	obs := &recordingLiveness{}
	src := NewOKXSource(OKXConfig{
		InstID:            "BTC-USDT",
		WSURL:             "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient:        srv.Client(),
		Liveness:          obs,
		HeartbeatInterval: time.Hour,
	})
	defer func() { _ = src.Close() }()

	if _, err := src.Recv(context.Background()); err == nil {
		t.Fatal("Recv accepted a rejected subscription")
	}
	if _, downs := obs.counts(); downs != 1 {
		t.Fatalf("attested %d breaks, want 1 — a socket that is up and delivering nothing must not "+
			"be recorded as coverage", downs)
	}
}

// A NIL Liveness IS A REAL STATE, NOT A DISABLED FEATURE. It must not panic, and
// it must attest nothing: the intervals read as UNKNOWN, which is honest for a
// feed nobody is vouching for.
func TestASourceWithoutALivenessSeamAttestsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.CloseNow()
	}))
	defer srv.Close()

	src := NewBinanceSource(BinanceConfig{
		Symbol:     "BTCUSDT",
		WSBase:     "ws://" + strings.TrimPrefix(srv.URL, "http://"),
		HTTPClient: srv.Client(),
	})
	defer func() { _ = src.Close() }()
	if _, err := src.Recv(context.Background()); err == nil {
		t.Fatal("Recv returned no error on a hung-up stream")
	}
}
