package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

func newTestServer() *Server {
	rd := &Readiness{}
	rd.Set(true)
	return New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// asPrincipal is what the api-gateway does to every forwarded request: it
// authenticates the caller and injects the mesh identity headers. This service
// authenticates nobody and is reachable only through the gateway, so this is the
// ONLY way a request here carries an identity (#409).
//
// It goes through auth.SetPrincipalHeaders rather than writing the header names
// by hand, because the names live in pkg/auth and a test that spelled them
// itself would keep passing the day the contract changed.
func asPrincipal(t *testing.T, s *Server, method, path, body, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	auth.SetPrincipalHeaders(req.Header, subject, "acme", []string{"pm"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestServer_Readyz(t *testing.T) {
	if rec := do(t, newTestServer(), http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz: got %d", rec.Code)
	}
}

func TestServer_Propose(t *testing.T) {
	// Min-variance over two equal-vol uncorrelated assets ⇒ ~50/50; current 100% A
	// ⇒ a sell of A and a buy of B.
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets map[string]float64
		Trades  []struct {
			InstrumentID string
			Side         int
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if d := resp.Targets["A"] - 0.5; d > 1e-2 || d < -1e-2 {
		t.Fatalf("target A should be ~0.5, got %.4f", resp.Targets["A"])
	}
	if len(resp.Trades) == 0 {
		t.Fatal("expected trades to move from 100%% A to 50/50")
	}
}

const ordersBody = `{"proposal":{"PortfolioID":"PF","MandateFeasible":true,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`

func TestServer_Orders(t *testing.T) {
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("orders: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count  int
		Orders []struct{ Issuer string }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Fatalf("want 1 order, got %d", resp.Count)
	}
	// THE ASSERTION THAT MATTERS. The issuer is stamped on the command and is what
	// the audit trail records as the person who moved the capital. It must be the
	// AUTHENTICATED caller — a 200 with somebody else's name on it is the defect.
	if resp.Orders[0].Issuer != "alice" {
		t.Errorf("issuer = %q, want the authenticated principal alice", resp.Orders[0].Issuer)
	}
}

// NO PRINCIPAL, NO ORDERS (#409). This service authenticates nobody: it trusts
// the gateway's injected headers, and that is sound ONLY because a NetworkPolicy
// makes the gateway its only reachable caller. A request arriving without a
// principal means either the caller bypassed the gateway or the gateway is
// misconfigured — and an anonymous caller must never be able to emit a command
// that moves capital, whichever it is.
func TestOrdersWithoutAPrincipalAreRefused(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodPost, "/v1/orders", ordersBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated caller got %d and, if it was 200, a command attributed to nobody: %s",
			rec.Code, rec.Body.String())
	}
}

// A BODY THAT NAMES ITS OWN ISSUER IS REFUSED, NOT OVERRIDDEN.
//
// Overriding silently would be worse than accepting it: a caller who sends an
// issuer and receives a 200 has been told their attribution was honoured, and it
// was not. Refusing says which field is not theirs to set.
func TestOrdersRefuseABodySuppliedIssuer(t *testing.T) {
	body := `{"issuer":"mallory","proposal":{"PortfolioID":"PF","MandateFeasible":true,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", body, "alice")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body naming another issuer got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authenticated principal") {
		t.Errorf("the refusal does not say why: %s", rec.Body.String())
	}
}

// An issuer echoing the caller's own subject is harmless and is allowed, so a
// client that round-trips the field is not broken by this. It still has no
// effect: the principal is what is used.
func TestOrdersAllowAnIssuerThatEchoesTheCaller(t *testing.T) {
	body := `{"issuer":"alice","proposal":{"PortfolioID":"PF","MandateFeasible":true,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", body, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestServer_Propose_BlackLitterman(t *testing.T) {
	// MaxSharpe (Type 2) over two equal-vol uncorrelated assets, equal market
	// weights, with a bullish absolute view on A (q=0.20). μ_BL tilts to A, so the
	// tangency target for A exceeds the 0.5 equilibrium.
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":2},
		"black_litterman":{"market_weights":[0.5,0.5],"risk_aversion":2.5,"tau":0.05,
			"views":[{"p":[1,0],"q":0.20,"omega":0}]},
		"current_weights":{"A":0.5,"B":0.5},"nav":100000,"prices":{"A":10,"B":10}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose+BL: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets map[string]float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Targets["A"] <= 0.5 {
		t.Fatalf("bullish BL view on A should raise A's target above 0.5, got %.4f", resp.Targets["A"])
	}
}

func TestServer_Propose_BlackLittermanInvalid(t *testing.T) {
	// A malformed BL block (risk_aversion 0) ⇒ 400 from the BL error path.
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":2},
		"black_litterman":{"market_weights":[0.5,0.5],"risk_aversion":0,"tau":0.05,
			"views":[{"p":[1,0],"q":0.20,"omega":0}]},
		"current_weights":{"A":0.5,"B":0.5},"nav":100000,"prices":{"A":10,"B":10}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed BL ⇒ 400, got %d", rec.Code)
	}
}

func TestServer_Propose_BodyTooLarge(t *testing.T) {
	// A body over the 8 MiB cap ⇒ 400 (MaxBytesReader makes the decoder error).
	big := strings.Repeat("A", (8<<20)+1024)
	body := `{"portfolio_id":"` + big + `","instruments":["A"],"covariance":[[0.04]],"objective":{"Type":1}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body ⇒ 400, got %d", rec.Code)
	}
}
