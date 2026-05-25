package gateway_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	querypb "github.com/kanz-eng/kanz-schemas-go/query/v1"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/gateway"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

// These are the API-01e gateway contract tests: they wire the full edge
// middleware chain over the gateway handler (against a fake upstream) and
// assert the cross-cutting behaviours — authz, rate limiting, version
// negotiation, and the query latency budget — that clients depend on.

const contractSecret = "contract-secret"

// chainedServer builds the production-shaped chain: version → auth → rate
// limit → idempotency, wrapping the gateway routes.
func chainedServer(t *testing.T, fc *fakeClient, perSec float64, burst int, requiredRole string) *httptest.Server {
	t.Helper()
	gwMux := http.NewServeMux()
	gateway.New(fc).Routes(gwMux)
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
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1"}}
	ts := chainedServer(t, fc, 0, 0, "risk.read")
	url := ts.URL + "/v1/portfolios/PF1/exposure"

	// No token ⇒ 401.
	if r := get(t, url, "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token ⇒ %d, want 401", r.StatusCode)
	}
	// Token without the required role ⇒ 403.
	noRole := mintContractJWT(t, "u", "acme", nil)
	if r := get(t, url, noRole, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("missing role ⇒ %d, want 403", r.StatusCode)
	}
	// Token with the role ⇒ 200.
	withRole := mintContractJWT(t, "u", "acme", []string{"risk.read"})
	if r := get(t, url, withRole, ""); r.StatusCode != http.StatusOK {
		t.Errorf("with role ⇒ %d, want 200", r.StatusCode)
	}
}

func TestContract_RateLimit429(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1"}}
	ts := chainedServer(t, fc, 1, 1, "risk.read") // 1 token, no refill in-test
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", "acme", []string{"risk.read"})

	if r := get(t, url, tok, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("first ⇒ %d, want 200", r.StatusCode)
	}
	if r := get(t, url, tok, ""); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second ⇒ %d, want 429", r.StatusCode)
	}
}

func TestContract_VersionMatrix(t *testing.T) {
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1"}}
	ts := chainedServer(t, fc, 0, 0, "risk.read")
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", "acme", []string{"risk.read"})

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
	fc := &fakeClient{exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1"}}
	ts := chainedServer(t, fc, 0, 0, "risk.read") // rate limit disabled
	url := ts.URL + "/v1/portfolios/PF1/exposure"
	tok := mintContractJWT(t, "u", "acme", []string{"risk.read"})

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

func mintContractJWT(t *testing.T, sub, tenant string, roles []string) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload := enc(map[string]any{"sub": sub, "tenant": tenant, "roles": roles})
	mac := hmac.New(sha256.New, []byte(contractSecret))
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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
