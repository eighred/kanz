package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/internal/gatewaysig"
)

// THE BOUNDARY THAT FAILED THREE TIMES, DRIVEN END TO END (#781).
//
// Signing had no test of its own. Its callers had theirs — the TUI client
// asserted the header it set, web-bff asserted the header it forwarded — and
// every one of those tests compared a signature against a signature the SAME
// FILE computed. That is a closed loop: it passes for any canonicalization,
// including a wrong one, and it is precisely why three callers shipped unable to
// talk to this middleware while their own suites were green (#198, #774, #777).
//
// These drive the real verifier with the real shared signer. Since #781 every
// caller signs through internal/gatewaysig, so proving gatewaysig against this
// middleware proves the callers against the gateway — which is the property the
// shared implementation exists to buy.
//
// WHAT THEY DO NOT CATCH, AND IT IS WORTH BEING EXACT ABOUT IT. Signer and
// verifier are now the same function, so a change to the canonicalization moves
// BOTH and every test here still passes. That was confirmed by mutation:
// dropping the trailing newline from gatewaysig.Sign leaves this file green.
// These tests prove the middleware still GOES THROUGH the shared function — a
// reverted, hand-rolled verifier fails them — and they pin which requests must
// be refused.
//
// The drift itself is caught by the two checks that do not share the
// implementation: gatewaysig's vector test, whose expected value was computed
// outside Go, and test/arch's cross-language check, which runs
// provision-tenant.sh's openssl pipeline and compares bytes. A shared
// implementation removes the drift between CALLERS; only an independent oracle
// can see the whole thing move at once.

func signingServer(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	h := Signing(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body to prove Signing restored it. A verifier that consumes the
		// request body authenticates correctly and hands the handler nothing —
		// which would surface as an empty order rather than as an auth failure.
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestTheSharedSignerIsAcceptedByThisVerifier(t *testing.T) {
	const secret = "shared-secret"
	srv := signingServer(t, secret)

	body := []byte(`{"quantity":"1","side":"BUY"}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	gatewaysig.SignRequest(req, []byte(secret), body)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("a request signed with gatewaysig.SignRequest was refused with %d. Every caller "+
			"on this platform signs through that function, so this is every caller refused — the "+
			"401 arrives BEFORE authentication and reads as an expired token (#781)", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("the handler saw body %q, want %q — Signing consumed the request body it verified",
			got, body)
	}
}

// TestAGetSignsOverTheTrailingNewline is the one-byte mistake that broke
// provision-tenant.sh (#774): a GET has no body, and the canonicalization still
// ends METHOD "\n" PATH "\n". A caller that stops after the path is wrong on
// every request it ever makes.
func TestAGetSignsOverTheTrailingNewline(t *testing.T) {
	const secret = "shared-secret"
	srv := signingServer(t, secret)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/portfolios/PF1/exposure", nil)
	if err != nil {
		t.Fatal(err)
	}
	gatewaysig.SignRequest(req, []byte(secret), nil)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a signed GET was refused with %d", res.StatusCode)
	}
}

// TestTheVerifierRefusesWhatItShould. Each case is a caller mistake that has
// happened or is one edit away, and each must be a 401 rather than a pass.
func TestTheVerifierRefusesWhatItShould(t *testing.T) {
	const secret = "shared-secret"
	srv := signingServer(t, secret)
	body := []byte(`{"quantity":"1"}`)

	cases := map[string]func(*http.Request){
		// #198, #774, #777: the caller never signed at all.
		"no signature at all": func(*http.Request) {},
		// An empty header is NOT the same as no header — the middleware reads it as
		// present-and-wrong, which is why SignRequest sets nothing on an empty key.
		"empty signature": func(r *http.Request) { r.Header.Set(gatewaysig.Header, "") },
		"wrong secret": func(r *http.Request) {
			gatewaysig.SignRequest(r, []byte("not-the-secret"), body)
		},
		// The classic: signing the full URI instead of the path.
		"signed over the query too": func(r *http.Request) {
			r.Header.Set(gatewaysig.Header, gatewaysig.Sign([]byte(secret), r.Method, r.URL.RequestURI(), body))
		},
		// The body changed after signing — a proxy that re-encodes JSON does this.
		"body edited after signing": func(r *http.Request) {
			gatewaysig.SignRequest(r, []byte(secret), []byte(`{"quantity":"999"}`))
		},
	}

	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/orders?dry=1", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			tamper(req)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401 — the verifier accepted a request it must refuse",
					res.StatusCode)
			}
		})
	}
}

// TestAnEmptySecretIsAPassThrough. A deployment that configures no signing
// secret is a legal posture, and it must not require callers to sign — the
// middleware is a no-op there. This is what makes provision-tenant.sh's
// "an empty secret contributes NO header" rule correct rather than merely
// tolerated.
func TestAnEmptySecretIsAPassThrough(t *testing.T) {
	srv := signingServer(t, "")

	res, err := http.Get(srv.URL + "/v1/portfolios")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d with no signing secret configured; an unsigned deployment must accept "+
			"unsigned requests", res.StatusCode)
	}
}
