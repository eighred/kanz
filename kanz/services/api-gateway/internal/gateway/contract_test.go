package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/devtoken"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// These are the API-01e gateway contract tests: they wire the full edge
// middleware chain over the gateway handler (against a fake upstream) and
// assert the cross-cutting behaviours — authz, rate limiting, version
// negotiation, and the query latency budget — that clients depend on.

const contractSecret = "contract-secret"

// contractTenant is the tenant these contract tokens are minted for, AND the
// tenant the canned replies must claim to be owned by — writeOwned compares the
// two and 404s a mismatch (#222). Named rather than repeated so the coupling is
// visible: change the token's tenant without changing the fixture and every
// contract test fails as a 404, which reads like a routing bug and is not one.
const contractTenant = "acme"

// chainedServer builds the production-shaped chain: version → auth → rate
// limit → idempotency, wrapping the gateway routes.
func chainedServer(t *testing.T, fc *fakeClient, perSec float64, burst int, requiredRole string) *httptest.Server {
	t.Helper()
	// The risk routes are READS (SEC-M2), and the contract's token carries requiredRole —
	// so that role is what grants Read here, exactly as cfg.RequiredRole does in main.
	gwMux := authz.NewMux(authz.Grants{requiredRole: {authz.Read}}, nil)
	gateway.New(fc, nil).Routes(gwMux)
	chain := middleware.Chain(
		middleware.Version(),
		middleware.Auth(middleware.NewJWTAuthenticator(contractSecret), requiredRole, nil),
		middleware.RateLimit(perSec, burst),
		middleware.Idempotency(time.Minute, 100),
	)
	ts := httptest.NewServer(chain(gwMux))
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url, token, version string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if version != "" {
		req.Header.Set("X-API-Version", version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestContract_AuthzRequired(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: contractTenant}}
	ts := chainedServer(t, fc, 0, 0, "risk.read")
	url := ts.URL + "/v1/portfolios/PF1/exposure"

	// No token ⇒ 401.
	if r := get(t, url, "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token ⇒ %d, want 401", r.StatusCode)
	}
	// Token without the required role ⇒ 403.
	noRole := mintContractJWT(t, "u", contractTenant, nil)
	if r := get(t, url, noRole, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("missing role ⇒ %d, want 403", r.StatusCode)
	}
	// Token with the role ⇒ 200.
	withRole := mintContractJWT(t, "u", contractTenant, []string{"risk.read"})
	if r := get(t, url, withRole, ""); r.StatusCode != http.StatusOK {
		t.Errorf("with role ⇒ %d, want 200", r.StatusCode)
	}
}

func TestContract_RateLimit429(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: contractTenant}}
	ts := chainedServer(t, fc, 1, 1, "risk.read") // 1 token, no refill in-test
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", contractTenant, []string{"risk.read"})

	if r := get(t, url, tok, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("first ⇒ %d, want 200", r.StatusCode)
	}
	if r := get(t, url, tok, ""); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second ⇒ %d, want 429", r.StatusCode)
	}
}

func TestContract_VersionMatrix(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: contractTenant}}
	ts := chainedServer(t, fc, 0, 0, "risk.read")
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", contractTenant, []string{"risk.read"})

	cases := map[string]int{"": http.StatusOK, "v1": http.StatusOK, "v2": http.StatusNotAcceptable}
	for ver, want := range cases {
		if r := get(t, url, tok, ver); r.StatusCode != want {
			t.Errorf("version %q ⇒ %d, want %d", ver, r.StatusCode, want)
		}
	}
}

// TestContract_QueryLatencyBudget guards against accidental per-request heavy
// work in the chain: p99 over 200 in-process queries must stay well under the
// gateway's budget. (In-process with a fake upstream this is microseconds; the
// test fails loudly if a regression adds a blocking call to the hot path.)
func TestContract_QueryLatencyBudget(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: contractTenant}}
	ts := chainedServer(t, fc, 0, 0, "risk.read") // rate limit disabled
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", contractTenant, []string{"risk.read"})

	const n = 200
	lat := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		r := get(t, url, tok, "")
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		lat[i] = time.Since(start)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d ⇒ %d", i, r.StatusCode)
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[int(float64(n)*0.99)]
	if p99 > 50*time.Millisecond {
		t.Errorf("p99 = %v exceeds 50ms in-process budget", p99)
	}
}

// --- helpers (shared with gateway_test.go in this package) -------------

// mintContractJWT builds the bearer token these contract cases authenticate
// with, through the ESTATE'S ONE MINTER rather than a local copy of the wire
// format.
//
// It used to assemble the JWS by hand, and #242 is why that stopped. The
// hand-rolled payload was `{sub, tenant, roles}` — no exp, no iss, no aud — and
// it authenticated, because the validator treated all three as optional. So the
// contract suite was pinning a token shape nothing else in the estate mints and
// asserting it worked, which is the strongest form of the defect: a second
// implementation of a security-relevant format, green, describing something no
// real caller does. Routing through devtoken.Mint means a claim the gateway
// starts requiring breaks the minter and the suite together, in the same
// change, instead of one of them quietly certifying the other.
func mintContractJWT(t *testing.T, sub, tenant string, roles []string) string {
	t.Helper()
	tok, err := devtoken.Mint(contractSecret, devtoken.Claims{
		Subject: sub,
		Tenant:  tenant,
		Roles:   roles,
		TTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("devtoken.Mint: %v", err)
	}
	return tok
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
