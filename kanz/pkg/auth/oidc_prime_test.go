package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-jose/go-jose/v4"
)

// CONSTRUCTION PROVES NOTHING ABOUT THE ISSUER (#457).
//
// NewOIDCAuthenticator does no network I/O — it checks that the issuer string is
// non-empty and that the key-age bounds are consistent, then returns. Every
// caller of it had therefore "configured OIDC" without anything having contacted
// the provider, and the gateway's composition root turned that into a startup log
// line reading "OIDC authentication enabled" beside an issuer that has never
// resolved.
//
// Prime is the eager form. These tests pin what it must actually do — not merely
// that it reaches the host, but that the provider it reaches is the one
// configured and hands over a usable key set.

func primeTestKeys(t *testing.T) jose.JSONWebKeySet {
	t.Helper()
	// A key set whose shape the fetcher accepts; the value never has to verify a
	// token here, only prove the fetch landed.
	var ks jose.JSONWebKeySet
	if err := json.Unmarshal([]byte(`{"keys":[{
		"kty":"OKP","crv":"Ed25519","kid":"k1",
		"x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
	}]}`), &ks); err != nil {
		t.Fatalf("unmarshal test JWKS: %v", err)
	}
	return ks
}

// primeServer serves discovery + JWKS. issuerOverride, when set, is what the
// discovery document CLAIMS to be — the impersonation case.
func primeServer(t *testing.T, issuerOverride string, fail *atomic.Bool) (url string, hits *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var n atomic.Int32
	ks := primeTestKeys(t)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		if fail != nil && fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		iss := srv.URL
		if issuerOverride != "" {
			iss = issuerOverride
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": iss, "jwks_uri": srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ks)
	})
	return srv.URL, &n
}

func primeAuth(t *testing.T, issuer string) *OIDCAuthenticator {
	t.Helper()
	a, err := NewOIDCAuthenticator(OIDCConfig{Issuer: issuer, Audience: "kanz-api"})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return a
}

func TestOIDCPrime_FetchesKeysUpFront(t *testing.T) {
	url, hits := primeServer(t, "", nil)
	a := primeAuth(t, url)

	// The state before Prime is the whole point: constructing reached nobody.
	if a.Primed() {
		if hits.Load() != 0 {
			t.Fatal("construction performed network I/O")
		}
		t.Fatal("a freshly constructed authenticator reported itself primed")
	}

	if err := a.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if !a.Primed() {
		t.Fatal("Prime succeeded but Primed() is false — the readiness gate would hold forever")
	}
	if hits.Load() == 0 {
		t.Fatal("Prime contacted nothing")
	}
}

// AN ISSUER THAT IS NOT THERE IS AN ERROR AT STARTUP, which is the entire
// purpose: the gateway shipped pointed at a hostname that does not resolve, and
// nothing said so until a user's first request.
func TestOIDCPrime_ReportsAnUnreachableIssuer(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	url, _ := primeServer(t, "", &down)
	a := primeAuth(t, url)

	if err := a.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded against an issuer that answered 503 — an unreachable provider " +
			"must be a startup fact, not a surprise on the first token")
	}
	if a.Primed() {
		t.Fatal("Primed() is true after a failed fetch — readiness would open on a gateway that " +
			"can verify nothing")
	}
}

// AND A HOST THAT DOES NOT RESOLVE AT ALL — the deployed case exactly, rather
// than a server returning an error status.
func TestOIDCPrime_ReportsAnIssuerThatDoesNotResolve(t *testing.T) {
	a := primeAuth(t, "https://this-host-does-not-exist.invalid")
	if err := a.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded against a hostname that cannot resolve")
	}
	if a.Primed() {
		t.Fatal("Primed() is true after a failed fetch")
	}
}

// A PROVIDER ANSWERING FOR A DIFFERENT ISSUER FAILS. This is what makes Prime a
// verification rather than a liveness ping: keys accepted from an issuer other
// than the configured one would validate tokens minted by somebody else, and a
// reachability check would call that healthy.
func TestOIDCPrime_RefusesAProviderClaimingAnotherIssuer(t *testing.T) {
	url, _ := primeServer(t, "https://someone-elses-idp.example", nil)
	a := primeAuth(t, url)

	err := a.Prime(context.Background())
	if err == nil {
		t.Fatal("Prime accepted a discovery document naming a DIFFERENT issuer")
	}
	if !strings.Contains(err.Error(), "someone-elses-idp.example") {
		t.Errorf("error %q does not name the mismatched issuer — an operator cannot tell this "+
			"from an ordinary outage", err)
	}
	if a.Primed() {
		t.Fatal("Primed() is true after an issuer mismatch")
	}
}

// PRIMING TWICE IS HARMLESS. The composition root retries on failure, and a
// success path that double-fetched would put avoidable load on the provider every
// rollout.
func TestOIDCPrime_IsIdempotent(t *testing.T) {
	url, hits := primeServer(t, "", nil)
	a := primeAuth(t, url)

	for i := range 3 {
		if err := a.Prime(context.Background()); err != nil {
			t.Fatalf("Prime #%d: %v", i+1, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("discovery was fetched %d times for three Prime calls, want 1 — the rate limiter "+
			"should suppress the repeats once keys are held", got)
	}
}
