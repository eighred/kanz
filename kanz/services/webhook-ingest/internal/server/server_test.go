package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
)

const secret = "topsecret"

// capturingPub records published events (Payload is the concrete proto).
type capturingPub struct{ events []bus.Event }

func (c *capturingPub) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

func (c *capturingPub) commandCount() int {
	n := 0
	for _, e := range c.events {
		if _, ok := e.Payload.(*orderpb.SubmitOrder); ok {
			n++
		}
	}
	return n
}

func newServer(t *testing.T) (*Server, *capturingPub) {
	t.Helper()
	pub := &capturingPub{}
	auth := ingest.NewAuthenticator(ingest.StaticSecrets{"momentum": secret}, nil, time.Minute, time.Now)
	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:      auth,
		Symbols:   ingest.StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    ingest.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    ingest.StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: ingest.StaticPositions{},
		Alloc: ingest.StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
			{Venue: "OKX", Weight: big.NewRat(4, 10)},
		}},
		Publisher: pub,
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	readiness := &Readiness{}
	readiness.Set(true)
	return New(readiness, nil, pipeline), pub
}

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func post(t *testing.T, srv *Server, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/tradingview", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.1:5555"
	if sig != "" {
		req.Header.Set("X-Signature", sig)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// okBody is a well-formed alert. It is a var rather than a const because #416
// made `ts` mandatory — an alert with no timestamp has no age, so the freshness
// bound refuses it, and every test here is about the HTTP surface rather than
// about that field.
//
// Stamped ONCE at package init, so the signature over it stays valid and the two
// calls in the duplicate-nonce test send byte-identical bodies. The bound is two
// minutes and this package runs in seconds.
var okBody = `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
	`"action":"buy","size":"5","size_type":"pct_of_equity","nonce":"srv-1",` +
	`"ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`

func TestWebhook_HappyPathFansOut(t *testing.T) {
	srv, pub := newServer(t)
	rec := post(t, srv, okBody, sign(okBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var resp struct {
		SignalID string   `json:"signal_id"`
		Orders   []string `json:"orders"`
		Count    int      `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 || len(resp.Orders) != 2 || resp.SignalID == "" {
		t.Fatalf("response = %+v, want 2 orders + signal_id", resp)
	}
	if pub.commandCount() != 2 {
		t.Fatalf("published %d commands, want 2", pub.commandCount())
	}
}

func TestWebhook_BadSignatureUnauthorized(t *testing.T) {
	srv, pub := newServer(t)
	rec := post(t, srv, okBody, sign("wrong")+"ff")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(pub.events) != 0 {
		t.Fatal("a rejected webhook published events")
	}
}

func TestWebhook_MissingSignatureUnauthorized(t *testing.T) {
	srv, _ := newServer(t)
	if rec := post(t, srv, okBody, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhook_DuplicateAcknowledged(t *testing.T) {
	srv, _ := newServer(t)
	if rec := post(t, srv, okBody, sign(okBody)); rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", rec.Code)
	}
	// A replayed identical webhook is acknowledged 200 (idempotent), not re-fanned.
	rec := post(t, srv, okBody, sign(okBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200", rec.Code)
	}
}

func TestWebhook_CloudflareOnlyRequiresHeader(t *testing.T) {
	// Rebuild the server with Cloudflare-only enforcement.
	base, pub := newServer(t)
	_ = base
	_ = pub
	pipeline := base.pipeline
	readiness := &Readiness{}
	readiness.Set(true)
	srv := New(readiness, nil, pipeline, WithCloudflareOnly(true))

	// No CF-Connecting-IP → rejected as public noise, before auth.
	if rec := post(t, srv, okBody, sign(okBody)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without CF-Connecting-IP = %d, want 401", rec.Code)
	}
	// With the header (transited the Cloudflare edge) → proceeds normally.
	req := httptest.NewRequest(http.MethodPost, "/webhook/tradingview", strings.NewReader(okBody))
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Signature", sign(okBody))
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("with CF-Connecting-IP = %d, want 202 (%s)", rec.Code, rec.Body)
	}
}

func TestHealthAndReady(t *testing.T) {
	srv, _ := newServer(t) // newServer marks the server ready
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, rec.Code)
		}
	}
}
