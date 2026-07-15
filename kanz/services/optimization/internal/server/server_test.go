package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestServer_Orders(t *testing.T) {
	body := `{"issuer":"alice","proposal":{"PortfolioID":"PF","MandateFeasible":true,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/orders", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("orders: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Fatalf("want 1 order, got %d", resp.Count)
	}
}

func TestServer_OrdersRequiresIssuer(t *testing.T) {
	body := `{"proposal":{"MandateFeasible":true,"Trades":[{"InstrumentID":"A","Side":1}]}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/orders", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing issuer must 400, got %d", rec.Code)
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
