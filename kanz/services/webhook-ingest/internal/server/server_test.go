package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/signal/translate"
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
	srv, pub, _ := newServerWithLog(t)
	return srv, pub
}

// srvAuthority is the binding this package's servers run under (#632).
//
// `momentum` is bound to fund-alpha, owned by tenant acme. `fund-victim` is a
// SECOND tenant's fund that momentum has no claim on — the target of the
// cross-tenant injection. Neither tenant is named after its fund, because the
// defect was precisely that the tenant WAS the fund id.
func srvAuthority(t *testing.T) ingest.FundAuthority {
	t.Helper()
	a, err := ingest.NewFundAuthority(
		map[string]string{"fund-alpha": "acme", "fund-victim": "globex"},
		map[string][]string{"momentum": {"fund-alpha"}},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	return a
}

// newServerWithLog also returns the buffer the server's logger writes to, so a
// test can assert that a refusal was actually REPORTED and not merely returned.
func newServerWithLog(t *testing.T) (*Server, *capturingPub, *bytes.Buffer) {
	t.Helper()
	pub := &capturingPub{}
	auth := ingest.NewAuthenticator(ingest.StaticSecrets{"momentum": secret}, nil, time.Minute, time.Now)
	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:      auth,
		Authority: srvAuthority(t),
		Symbols:   ingest.StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    ingest.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity: ingest.StaticEquity{
			"fund-alpha":  big.NewRat(1_000_000, 1),
			"fund-victim": big.NewRat(1_000_000, 1),
		},
		Positions: ingest.StaticPositions{},
		Alloc: ingest.StaticAllocation{
			"fund-alpha": {
				{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
				{Venue: "OKX", Weight: big.NewRat(4, 10)},
			},
			// The victim fund is FULLY TRADEABLE by this deployment — priced, funded
			// and allocated. That is the point: the only thing standing between a
			// signed alert and this fund's book is the strategy→fund binding, so a
			// test that passed because the fund was merely unconfigured would prove
			// nothing (ErrNoAllocation already covered that case, and #632 was live
			// anyway).
			"fund-victim": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}},
		},
		Publisher: pub,
		Gate:      translate.OpenGate(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	readiness := &Readiness{}
	readiness.Set(true)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(readiness, logger, pipeline), pub, &logs
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

// THE ATTACK, RUN THROUGH THE PUBLIC SURFACE (#632).
//
// A party holding strategy `momentum`'s webhook secret POSTs a CORRECTLY SIGNED
// alert naming `fund-victim` — a fund this deployment serves, priced, funded and
// allocated, belonging to tenant globex. Nothing about the request is malformed
// and the HMAC verifies, because the HMAC only ever proved the strategy.
//
// Before the fix this returned 202, wrote a StrategySignal FACT under tenant
// `fund-victim`, and published a SubmitOrder on
// `tenant.fund-victim.order.order.submit` — the subject globex's own OMS
// consumes. The audit trail attributed the trade to the victim.
func TestPerimeter_ASignedAlertForAnotherTenantsFundIsForbidden(t *testing.T) {
	srv, pub, logs := newServerWithLog(t)
	attack := `{"strategy_id":"momentum","fund_id":"fund-victim","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"5","size_type":"pct_of_equity","nonce":"inject-1",` +
		`"ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`

	rec := post(t, srv, attack, sign(attack))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s), want 403.\n\n"+
			"A correctly signed alert named ANOTHER TENANT'S FUND and was not refused as "+
			"unauthorized. The HMAC authenticates the STRATEGY; fund_id is caller-supplied.",
			rec.Code, rec.Body)
	}
	// NOTHING may have reached the bus — not the order command, and not the audit
	// FACT either: a StrategySignal written under the victim's tenant is itself the
	// cross-tenant write.
	if len(pub.events) != 0 {
		t.Fatalf("%d event(s) published for a refused alert, want 0 — a signal FACT or a "+
			"SubmitOrder reached the victim tenant's book", len(pub.events))
	}
	// AND THE REFUSAL MUST BE VISIBLE. A silent 403 on the platform's public
	// entrance is a leaked strategy secret nobody finds out about.
	out := logs.String()
	if !strings.Contains(out, "not bound to") {
		t.Errorf("nothing was logged about the refusal.\n\nlog output: %q\n\n"+
			"This is the signature of a leaked strategy secret being pointed at another "+
			"tenant's book. It has to reach an operator, not just the sender.", out)
	}
	for _, want := range []string{"momentum", "fund-victim"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not name %q — an operator cannot tell which sender, or which "+
				"fund it reached for.\n\nlog output: %q", want, out)
		}
	}
	// The response body must NOT say which funds exist: an unauthorized caller
	// must not be able to enumerate the estate by probing fund ids.
	if strings.Contains(rec.Body.String(), "fund-victim") {
		t.Errorf("the response names the fund (%s) — that turns this endpoint into a "+
			"fund-id oracle", rec.Body.String())
	}
}

// AND A REDELIVERY OF THAT PROBE IS A DUPLICATE, NOT A FRESH ATTEMPT.
//
// The refusal is a VERDICT — the binding table is static config read at startup,
// so retrying can never succeed — which means it must BURN the nonce. Releasing
// it would let a prober re-enter the whole pipeline on every retry.
func TestPerimeter_AForbiddenAlertBurnsItsNonce(t *testing.T) {
	srv, _, _ := newServerWithLog(t)
	attack := `{"strategy_id":"momentum","fund_id":"fund-victim","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"5","size_type":"pct_of_equity","nonce":"inject-2",` +
		`"ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	sig := sign(attack)

	if rec := post(t, srv, attack, sig); rec.Code != http.StatusForbidden {
		t.Fatalf("first attempt = %d, want 403", rec.Code)
	}
	// 200 "duplicate ignored" is what a burnt nonce looks like at this surface.
	if rec := post(t, srv, attack, sig); rec.Code != http.StatusOK {
		t.Fatalf("redelivery = %d, want 200 (duplicate ignored) — the nonce was RELEASED, so "+
			"every retry of a permanently-refused alert re-enters the whole pipeline", rec.Code)
	}
}

// THE BOUND FUND STILL TRADES, on the same server as the test above. Without
// this, a fix that refused everything would pass — and a total ingest outage is
// not a repair.
func TestPerimeter_TheBoundFundStillTradesAndCarriesItsConfiguredTenant(t *testing.T) {
	srv, pub, _ := newServerWithLog(t)
	alert := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"5","size_type":"pct_of_equity","nonce":"bound-1",` +
		`"ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`

	if rec := post(t, srv, alert, sign(alert)); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202 — the bound strategy/fund pair was refused",
			rec.Code, rec.Body)
	}
	var cmds int
	for _, e := range pub.events {
		if _, ok := e.Payload.(*orderpb.SubmitOrder); !ok {
			continue
		}
		cmds++
		// THE TENANT CAME FROM CONFIGURATION, not from the request. `acme` appears
		// nowhere in the alert; `fund-alpha` does. If these were ever the same string
		// this assertion would be satisfied by the defect.
		if e.TenantID != "acme" {
			t.Errorf("command tenant_id = %q, want acme (the tenant configured for fund-alpha)", e.TenantID)
		}
		if want := "tenant.acme." + translate.SubjectSubmit; e.Subject != want {
			t.Errorf("wire subject = %q, want %q — the broker routes on this, so it is what "+
				"decides whose OMS executes the order", e.Subject, want)
		}
		if strings.Contains(e.Subject, "fund-alpha") {
			t.Errorf("the wire subject carries the FUND ID (%q). That is the #632 routing: the "+
				"caller chose the subject their order was delivered on", e.Subject)
		}
	}
	if cmds != 2 {
		t.Fatalf("%d order commands, want 2 — this test asserts on nothing", cmds)
	}
}

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
