package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/optimization"
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

// ordersBody is a proposal in the shape a client gets back from /v1/propose: no
// mandate verdict on it, because nothing checked it. It used to carry
// "MandateFeasible":true, which is what every proposal this service built said
// about itself (#646).
const ordersBody = `{"proposal":{"PortfolioID":"PF",
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`

// A PROPOSAL NOTHING CHECKED DOES NOT BECOME ORDERS (#646).
//
// This request used to return 200 and a live SubmitOrder command. The verdict
// bridge.ToOrders gates on was stamped true by RebalanceProposal's constructor,
// and optimization.CheckMandate had never run anywhere in this platform — so the
// gate on whether a rebalance becomes capital commands was answered by a field
// nobody had computed.
func TestOrdersRefuseAProposalNoMandateCheckHasRun(t *testing.T) {
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unchecked proposal got %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error   string
		Verdict string
		Detail  string
		Count   int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 0 {
		t.Fatalf("%d order(s) were returned for a proposal no mandate check has run on", resp.Count)
	}
	if resp.Verdict != "UNCHECKED" {
		t.Errorf("verdict = %q, want UNCHECKED — the refusal must say which of the three states "+
			"the proposal was in, or an operator cannot tell 'nothing checked it' from 'it breached'",
			resp.Verdict)
	}
	if !strings.Contains(resp.Error, "mandate") {
		t.Errorf("the refusal does not say why: %s", rec.Body.String())
	}
}

// THE ISSUER IS STILL BOUND TO THE AUTHENTICATED PRINCIPAL. This assertion used
// to live on this route's 200; no request can reach a 200 here while the service
// has no mandate source (#646), so it moved to autopublish_test.go, which drives
// Server.materialize with an approved proposal. It is named here so a reader
// looking for the AUTH-01c coverage on this route finds where it went.
func TestOrdersBindTheIssuerToThePrincipal(t *testing.T) {
	rec := materializeAs(t, newTestServer(), feasibleProposal(), "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("an approved proposal must materialize: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count  int
		Orders []struct{ Issuer string }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Fatalf("want 1 order, got %d", resp.Count)
	}
	if resp.Orders[0].Issuer != "alice" {
		t.Errorf("issuer = %q, want the authenticated principal alice", resp.Orders[0].Issuer)
	}
}

// A BODY THAT CERTIFIES ITS OWN PROPOSAL IS REFUSED, NOT OVERRIDDEN (#646).
//
// RebalanceProposal has no json tags and ordersRequest.Proposal is a plain
// decode of it, so the mandate verdict arrived off the request body — in the
// same struct as the comment on ordersRequest.Issuer explaining that this
// service authenticates nobody and therefore cannot accept an issuer from one.
// #409 moved the issuer to the principal and left the constraint verdict where
// the issuer had been.
func TestOrdersRefuseABodySuppliedMandateVerdict(t *testing.T) {
	cases := map[string]string{
		"the current spelling":      `{"proposal":{"PortfolioID":"PF","MandateStatus":"FEASIBLE","Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`,
		"the pre-#646 spelling":     `{"proposal":{"PortfolioID":"PF","MandateFeasible":true,"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`,
		"json's case-insensitivity": `{"proposal":{"PortfolioID":"PF","mandatestatus":"feasible","Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`,
		"a self-declared clean bill": `{"proposal":{"PortfolioID":"PF","Violations":[],"MandateStatus":"FEASIBLE",
			"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", body, "alice")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a body carrying its own mandate verdict got %d, want 400: %s",
					rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "must not be supplied in the body") {
				t.Errorf("the refusal does not say which field is not the caller's to set: %s",
					rec.Body.String())
			}
		})
	}
}

// A LOWER-CASE OR MIXED-CASE VERDICT IS NOT A LOOPHOLE. encoding/json matches
// keys case-insensitively, so a check that compared field names exactly would
// refuse "MandateStatus" and admit "mandatestatus" — which the decoder honours.
func TestASuppliedVerdictIsDecodedWhateverItsCase(t *testing.T) {
	body := []byte(`{"proposal":{"mandatestatus":"feasible"}}`)
	var req ordersRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Proposal.MandateStatus != optimization.MandateFeasible {
		t.Fatalf("non-vacuity: the decoder did not honour the lower-case key, so the refusal "+
			"below proves nothing; got %s", req.Proposal.MandateStatus)
	}
	if _, supplied := suppliedMandateVerdict(body); !supplied {
		t.Fatal("a lower-case verdict key was not seen as supplied, and the decoder honours it")
	}
}

// AN ECHOED /v1/propose RESPONSE IS REFUSED FOR THE RIGHT REASON. A client that
// round-trips the proposal it was handed is not doing anything wrong, so it must
// be told what is actually missing (nothing checked this) rather than being sent
// away over a field it merely copied back.
func TestARoundTrippedProposalIsRefusedAsUncheckedNotAsForged(t *testing.T) {
	s := newTestServer()
	proposeBody := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}}`
	proposed := do(t, s, http.MethodPost, "/v1/propose", proposeBody)
	if proposed.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", proposed.Code, proposed.Body.String())
	}
	var asSeen struct{ MandateStatus string }
	if err := json.Unmarshal(proposed.Body.Bytes(), &asSeen); err != nil {
		t.Fatal(err)
	}
	if asSeen.MandateStatus != "UNCHECKED" {
		t.Fatalf("/v1/propose reported MandateStatus %q — it consults no mandate and must say so",
			asSeen.MandateStatus)
	}

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders",
		`{"proposal":`+proposed.Body.String()+`}`, "alice")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an echoed proposal got %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "must not be supplied in the body") {
		t.Error("a client echoing the proposal it was given was blamed for supplying a verdict; " +
			"UNCHECKED asserts nothing and must pass through to the refusal that names the real problem")
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
	body := `{"issuer":"mallory","proposal":{"PortfolioID":"PF",
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
//
// The assertion is that the request is NOT refused as a forged issuer. It goes
// on to be refused for want of a mandate check (#646), which is a different
// answer with a different status, and conflating the two would let the issuer
// rule rot behind it.
func TestOrdersAllowAnIssuerThatEchoesTheCaller(t *testing.T) {
	body := `{"issuer":"alice","proposal":{"PortfolioID":"PF",
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100}]}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/orders", body, "alice")
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("an issuer echoing the caller was refused as forged: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422 (unchecked mandate): %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "forged-issuer") {
		t.Errorf("the refusal blames the issuer: %s", rec.Body.String())
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
