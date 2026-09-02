package server

import (
	"time"

	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/optimization"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/optimization/internal/bridge"
)

func newTestServer() *Server {
	rd := &Readiness{}
	rd.Set(true)
	return New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)), testFreshness())
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
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
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
// testAsOf is the knowledge horizon every fixture proposal below is dated at, and
// the instant the servers in these tests are clocked to (#970).
//
// FIXED RATHER THAN time.Now(): the freshness gate compares the two, so a fixture
// dated "now" and a server clocked to the real "now" would make these tests
// depend on how long the suite takes to reach them. Both sides are pinned so the
// gate is never what a mandate test is measuring — the gate has its own tests in
// bridge/freshness_test.go.
const testAsOf = "2026-09-02T12:00:00Z"

func testClock() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }

// testFreshness is a bound wide enough to admit testAsOf. Without it the zero
// Freshness refuses everything and every assertion in this package would pass
// against a route that materializes nothing.
func testFreshness() Option {
	return WithProposalFreshness(bridge.Freshness{MaxAge: time.Hour, Now: testClock})
}

// testEnvelope is a permissive but PRESENT constraint envelope (#972). Present,
// because ToOrders refuses an unbounded proposal and every assertion in this
// package would otherwise pass against a route that materializes nothing.
// Permissive, because the envelope is never what a mandate or issuer test is
// measuring — it has its own tests in bridge/constraints_test.go.
const testEnvelope = `"Constraints":{"MaxNotional":{"coefficient":100000000,"exponent":0}}`

const ordersBody = `{"proposal":{"PortfolioID":"PF","AsOf":"` + testAsOf + `",` + testEnvelope + `,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100,"Notional":1000}]}}`

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
	// A REAL-CLOCK BOUND FOR THIS ONE TEST. /v1/propose stamps AsOf with the
	// wall clock, so a server pinned to testClock would see its own freshly
	// built proposal as dated in the future and refuse it as stale — measuring
	// the harness rather than the issuer rule this test is about (#970).
	s := New(func() *Readiness { r := &Readiness{}; r.Set(true); return r }(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithProposalFreshness(bridge.Freshness{MaxAge: time.Hour}))

	proposeBody := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}}`
	proposed := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody, "user:pm")
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
	body := `{"issuer":"alice","proposal":{"PortfolioID":"PF","AsOf":"` + testAsOf + `",` + testEnvelope + `,
		"Trades":[{"InstrumentID":"A","Side":1,"Quantity":100,"Notional":1000}]}}`
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
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
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
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed BL ⇒ 400, got %d", rec.Code)
	}
}

func TestServer_Propose_BodyTooLarge(t *testing.T) {
	// A body over the 8 MiB cap ⇒ 400 (MaxBytesReader makes the decoder error).
	big := strings.Repeat("A", (8<<20)+1024)
	body := `{"portfolio_id":"` + big + `","instruments":["A"],"covariance":[[0.04]],"objective":{"Type":1}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body ⇒ 400, got %d", rec.Code)
	}
}

// AN ALL-ZERO COVARIANCE MUST NOT COME BACK AS A COMPUTED ZERO RISK (#621).
//
// This is the mutation #621 was filed with: the request below asserts that
// neither instrument moves and neither pair co-moves. The matrix is symmetric
// and PSD, so it passed every check the optimizer had, and the response carried
// "ExpectedRisk":0 — the same bytes a genuinely riskless book produces.
func TestProposeDoesNotReportZeroRiskFromAZeroCovariance(t *testing.T) {
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0,0],[0,0]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	// THE RAW BYTES, not a struct: a *float64 decoded into a float64 field would
	// read null back as 0 and this test would assert nothing.
	raw := rec.Body.String()
	if strings.Contains(raw, `"ExpectedRisk":0`) {
		t.Fatalf("the response reports a computed zero risk for a covariance that carries no "+
			"information: %s", raw)
	}
	var resp struct {
		ExpectedRisk      *float64
		CovarianceQuality string
		Targets           map[string]float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// NON-VACUITY: the optimization really ran and produced a book, so the nil
	// below is a withheld number rather than an error path.
	if len(resp.Targets) != 2 {
		t.Fatalf("expected targets for both instruments, got %v", resp.Targets)
	}
	if resp.ExpectedRisk != nil {
		t.Fatalf("ExpectedRisk is %v — an absence of data reached the response as a risk number",
			*resp.ExpectedRisk)
	}
	if resp.CovarianceQuality != "RANK_DEFICIENT" {
		t.Fatalf("CovarianceQuality is %q, want RANK_DEFICIENT — a null risk with nothing "+
			"explaining it is only half the answer", resp.CovarianceQuality)
	}
}

// The observation count reaches the optimizer from the request body, and a
// covariance NOBODY VOUCHED FOR is a different answer from one checked against
// its own sample size.
func TestProposeReportsWhetherTheCovarianceWasVouchedFor(t *testing.T) {
	const base = `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}%s}`
	cases := []struct {
		name  string
		extra string
		want  string
		risk  bool
	}{
		{"no observation count stated", "", "FULL_RANK", true},
		{"250 observations over 2 assets", `,"observations":250`, "OBSERVED", true},
		{"2 observations over 2 assets", `,"observations":2`, "UNDER_OBSERVED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", fmt.Sprintf(base, tc.extra), "user:pm")
			if rec.Code != http.StatusOK {
				t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				ExpectedRisk      *float64
				CovarianceQuality string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.CovarianceQuality != tc.want {
				t.Fatalf("CovarianceQuality: got %q want %q", resp.CovarianceQuality, tc.want)
			}
			if got := resp.ExpectedRisk != nil; got != tc.risk {
				t.Fatalf("risk reported = %v, want %v (quality %s)", got, tc.risk, resp.CovarianceQuality)
			}
			// Σ = diag(0.04, 0.04) ⇒ min-variance 50/50 ⇒ wᵀΣw = 2·0.25·0.04 = 0.02,
			// √0.02 = 0.1414213562373095. Derived from the definition, not from the
			// implementation.
			if tc.risk {
				if d := *resp.ExpectedRisk - 0.1414213562373095; d > 1e-6 || d < -1e-6 {
					t.Fatalf("ExpectedRisk %.12f, want 0.1414213562373095", *resp.ExpectedRisk)
				}
			}
		})
	}
}

// A SINGULAR COVARIANCE DOES NOT COME BACK AS AN EQUAL-WEIGHT TANGENCY PORTFOLIO (#621).
//
// Two perfectly correlated assets and a max-Sharpe objective: solveLinear cannot
// factor Σ, and maxSharpe used to substitute its own 1/n seed and return
// {"A":0.5,"B":0.5} with a 200 and no flag on it.
func TestProposeRefusesAnUndefinedTangencyPortfolio(t *testing.T) {
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0.04],[0.04,0.04]],
		"expected_returns":[0.05,0.10],
		"objective":{"Type":2},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10}}`
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", body, "user:pm")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a singular Σ under MaxSharpe got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if raw := rec.Body.String(); !strings.Contains(raw, "tangency") {
		t.Fatalf("the refusal must say what it could not compute, got %s", raw)
	}
	if raw := rec.Body.String(); strings.Contains(raw, `"Targets"`) {
		t.Fatalf("a refused optimization returned a portfolio: %s", raw)
	}
}

// A STALE PROPOSAL IS REFUSED AT THE ROUTE, WITH ITS OWN REASON (#970).
//
// The route is where an operator meets this refusal, and the status and reason
// class are what they act on. 409 rather than 422: the request is well formed and
// the caller was entitled to make it — the proposal is simply no longer valid
// against the book, and the fix is to compute a new one rather than to correct
// the request.
func TestOrdersRefuseAStaleProposalWithItsOwnReason(t *testing.T) {
	rd := &Readiness{}
	rd.Set(true)
	// A bound of one minute against a fixture dated an hour before the clock.
	s := New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithProposalFreshness(bridge.Freshness{MaxAge: time.Minute, Now: func() time.Time {
			return testClock().Add(time.Hour)
		}}))

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale proposal got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Reason string `json:"reason"`
		AsOf   string `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Reason != "STALE_PROPOSAL" {
		t.Fatalf("reason = %q, want STALE_PROPOSAL — the refusal CLASS is what a dashboard counts, "+
			"and grepping English out of the message is how a reworded sentence empties a panel",
			got.Reason)
	}
	if got.AsOf != testAsOf {
		t.Fatalf("as_of = %q, want %q — the horizon must be echoed so the operator can see how "+
			"stale it was", got.AsOf, testAsOf)
	}
}

// AN UNCONFIGURED BOUND REFUSES, AND SAYS SO AS ITS OWN PROBLEM.
//
// This is the fail-closed direction: a deployment that forgot the setting must
// not silently materialize everything. And it must NOT be reported as
// STALE_PROPOSAL — nothing is wrong with the proposal, so telling the caller to
// re-run the optimizer would send them round a loop that refuses forever.
func TestOrdersRefuseWhenNoFreshnessBoundIsConfigured(t *testing.T) {
	rd := &Readiness{}
	rd.Set(true)
	s := New(rd, slog.New(slog.NewTextHandler(io.Discard, nil))) // no WithProposalFreshness

	rec := asPrincipal(t, s, http.MethodPost, "/v1/orders", ordersBody, "alice")
	if rec.Code != http.StatusConflict {
		t.Fatalf("an unconfigured deployment got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Reason != "FRESHNESS_UNCONFIGURED" {
		t.Fatalf("reason = %q, want FRESHNESS_UNCONFIGURED — an unset bound is a deployment "+
			"problem, not a stale proposal", got.Reason)
	}
	if !strings.Contains(got.Detail, "OPTIMIZATION_PROPOSAL_MAX_AGE") {
		t.Fatalf("the detail does not name the setting to fix: %q", got.Detail)
	}
}
