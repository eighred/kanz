package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

// THE GATEWAY REFUSES AN UNSIGNED REQUEST BEFORE IT AUTHENTICATES (#777), so a
// BFF that forwards only a bearer token produces a web app whose login succeeds
// and whose every data screen 401s. These tests stand a fake gateway that
// verifies the signature exactly as middleware.Signing does, because the failure
// this guards against is precisely a signature that is absent or computed over
// the wrong bytes -- both of which a fake that merely records the header would
// accept.

// signingGateway verifies API-01d the way the real middleware does and reports
// what it saw, so a test can tell "no signature" from "wrong signature".
type signingGateway struct {
	secret []byte
	sawSig string
	sawPth string
	ok     bool
}

func (g *signingGateway) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.sawSig = r.Header.Get("X-Signature")
		g.sawPth = r.URL.Path
		if g.sawSig == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing request signature"}`))
			return
		}
		mac := hmac.New(sha256.New, g.secret)
		mac.Write([]byte(r.Method + "\n" + r.URL.Path + "\n"))
		mac.Write(body)
		want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(g.sawSig)) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid request signature"}`))
			return
		}
		g.ok = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProxiedGetIsSigned is the defect itself: before the fix this returns 401
// "missing request signature", which is what every screen in the web app saw.
func TestProxiedGetIsSigned(t *testing.T) {
	gw := &signingGateway{secret: []byte("s3cr3t")}
	gwSrv := gw.start(t)
	srv := bffWithSigning(t, gwSrv.URL, "s3cr3t")

	rec := proxyAs(t, srv, http.MethodGet, "/api/v1/portfolios/PF1/exposure", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("proxied GET = %d (%s), want 200 — the gateway refused the BFF's own request",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if !gw.ok {
		t.Error("the gateway did not accept the signature")
	}
	// THE PATH SIGNED MUST BE THE GATEWAY'S. Signing "/api/v1/..." yields a
	// perfectly valid signature for a request that is never sent, and the
	// symptom is identical to not signing at all.
	if gw.sawPth != "/v1/portfolios/PF1/exposure" {
		t.Errorf("gateway saw path %q, want the /v1 path with the /api prefix stripped", gw.sawPth)
	}
}

// TestProxiedPostSignsOverTheBody catches a signature computed over an empty or
// already-consumed body: r.Clone shares the original ReadCloser, so reading it
// to sign and not restoring it forwards nothing while the signature commits to
// the real bytes.
func TestProxiedPostSignsOverTheBody(t *testing.T) {
	gw := &signingGateway{secret: []byte("s3cr3t")}
	gwSrv := gw.start(t)
	srv := bffWithSigning(t, gwSrv.URL, "s3cr3t")

	rec := proxyAs(t, srv, http.MethodPost, "/api/v1/orders", `{"order_id":"A-1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxied POST = %d (%s), want 200 — the body and its signature disagree",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// TestAWrongSecretIsRefusedNotAccepted is the non-vacuity arm: it proves the
// fake gateway can still say no, so the two passes above are the signature being
// right rather than the check being asleep.
func TestAWrongSecretIsRefusedNotAccepted(t *testing.T) {
	gw := &signingGateway{secret: []byte("what-the-gateway-holds")}
	gwSrv := gw.start(t)
	srv := bffWithSigning(t, gwSrv.URL, "what-the-bff-holds")

	rec := proxyAs(t, srv, http.MethodGet, "/api/v1/portfolios/PF1/exposure", "")

	if rec.Code == http.StatusOK {
		t.Fatal("a mismatched signing secret was ACCEPTED — this test proves nothing")
	}
	if gw.sawSig == "" {
		t.Error("no signature was sent at all; this arm is meant to exercise a WRONG one")
	}
}

// TestNoSecretSendsNoHeader keeps the local-development posture working: a
// gateway with signing disabled must still be reachable, and an empty
// X-Signature is NOT the same request as no header — the middleware reads an
// empty value as a present-but-wrong signature.
func TestNoSecretSendsNoHeader(t *testing.T) {
	var saw string
	var present bool
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("X-Signature")
		_, present = r.Header["X-Signature"]
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gwSrv.Close)
	srv := bffWithSigning(t, gwSrv.URL, "")

	if rec := proxyAs(t, srv, http.MethodGet, "/api/v1/portfolios/PF1/exposure", ""); rec.Code != http.StatusOK {
		t.Fatalf("unsigned proxy against a signing-disabled gateway = %d, want 200", rec.Code)
	}
	if present {
		t.Errorf("sent X-Signature=%q with no secret configured — an empty signature is a WRONG "+
			"signature to the gateway, not an absent one", saw)
	}
}

// bffWithSigning builds a BFF whose proxy targets gatewayURL and signs with
// secret. An empty secret is the signing-disabled posture, not a broken one.
func bffWithSigning(t *testing.T, gatewayURL, secret string) *Server {
	t.Helper()
	ip, err := clientip.NewResolver("CF-Connecting-IP", []string{"192.0.2.1"})
	if err != nil {
		t.Fatalf("clientip: %v", err)
	}
	srv, err := New(&Readiness{}, Options{
		Identity:      identityclient.New("http://identity.invalid", "X-Kanz-Client-IP", time.Second),
		ClientIP:      ip,
		Sessions:      session.NewManager(time.Hour),
		GatewayURL:    gatewayURL,
		SigningSecret: secret,
		SecureCookies: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// proxyAs issues a proxied call carrying a real session cookie, because
// handleProxy refuses before it ever reaches the gateway without one — an
// unauthenticated probe would 401 for the wrong reason and prove nothing.
func proxyAs(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	id, err := srv.sessions.Create(session.Session{
		AccessToken: "the-server-held-token",
		Subject:     "user:fiona",
		Tenant:      "acme",
		Expiry:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}
