package gatewaysig

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE CANONICALIZATION IS PINNED TO A VECTOR, not merely to itself (#781).
//
// A round-trip test — Sign then Verify — passes for ANY canonicalization,
// including a wrong one, because both halves change together. That is exactly
// the property that made this defect survive: four independent statements of the
// rule, each internally consistent, and the disagreement only observable by
// crossing the boundary.
//
// So the vector below is computed OUTSIDE this package and outside Go. It is
// what `openssl dgst -sha256 -hmac s3cr3t | base64 | tr '+/' '-_' | tr -d '=\n'`
// produces for the same input — the same pipeline
// infra/onboarding/provision-tenant.sh runs — and
// test/arch/gateway_signature_test.go re-derives it from that script on every
// run rather than trusting this constant.
const (
	vectorKey    = "s3cr3t"
	vectorMethod = "GET"
	vectorPath   = "/v1/portfolios/PF1/exposure"
	vectorSig    = "KoRKew4XL6mGcHGT3WAUJeVHxIDTRQGCehOHTnm6Plc"
)

func TestSignMatchesTheIndependentlyComputedVector(t *testing.T) {
	got := Sign([]byte(vectorKey), vectorMethod, vectorPath, nil)
	if got != vectorSig {
		t.Fatalf("Sign = %q, want %q.\n\nThe canonicalization changed. Every caller on this "+
			"platform and the gateway's own verifier now disagree with every signature already in "+
			"flight, and the symptom is a 401 that reads as an authentication failure because it "+
			"IS one — middleware.Signing answers before Auth runs (#781).", got, vectorSig)
	}
}

// TestTheEncodingIsUnpaddedBase64URL. SHA-256 is 32 bytes, which base64-encodes
// to 44 characters with exactly one '=' of padding — so the difference between
// StdEncoding and RawURLEncoding is present on EVERY signature this platform
// ever produces, not on an unlucky subset. A caller reaching for the obvious
// base64.StdEncoding is wrong every time, which is the good case; the dangerous
// version of this mistake is one that is right most of the time.
func TestTheEncodingIsUnpaddedBase64URL(t *testing.T) {
	got := Sign([]byte(vectorKey), "POST", "/v1/orders", []byte(`{"x":1}`))

	if strings.Contains(got, "=") {
		t.Errorf("signature %q carries base64 padding; the gateway decodes RawURLEncoding", got)
	}
	if strings.ContainsAny(got, "+/") {
		t.Errorf("signature %q uses standard base64 alphabet; the gateway expects base64URL "+
			"('-' and '_')", got)
	}
	if len(got) != 43 {
		t.Errorf("signature %q is %d characters; an unpadded base64url SHA-256 is always 43",
			got, len(got))
	}
}

// TestANilBodyIsTheEmptyBody. A GET has no body, and callers spell that as nil,
// as []byte{}, or by not setting one. All three must sign identically or the
// same request signs differently depending on how the caller happened to
// represent "nothing".
func TestANilBodyIsTheEmptyBody(t *testing.T) {
	nilBody := Sign([]byte(vectorKey), "GET", "/v1/x", nil)
	empty := Sign([]byte(vectorKey), "GET", "/v1/x", []byte{})
	if nilBody != empty {
		t.Fatalf("nil body signs %q and an empty slice signs %q", nilBody, empty)
	}
}

// TestTheQueryIsNotSigned pins the trap a caller falls into by reaching for
// r.URL.String() or RequestURI instead of Path. The gateway hashes r.URL.Path,
// so a signature computed over the query is refused — and the 401 says nothing
// about which half was wrong.
func TestTheQueryIsNotSigned(t *testing.T) {
	bare := Sign([]byte(vectorKey), "GET", "/v1/orders", nil)
	withQuery := Sign([]byte(vectorKey), "GET", "/v1/orders?limit=10", nil)
	if bare == withQuery {
		t.Fatal("a path and the same path with a query sign identically — then the signature is " +
			"not over the path the gateway hashes")
	}

	// And SignRequest takes the PATH from the URL, never the raw query, so a
	// caller that builds a query-bearing request is signed correctly without
	// having to know this.
	req := httptest.NewRequest(http.MethodGet, "http://gw/v1/orders?limit=10", nil)
	SignRequest(req, []byte(vectorKey), nil)
	if got := req.Header.Get(Header); got != bare {
		t.Fatalf("SignRequest signed %q, want the path-only %q — it is reading more than "+
			"r.URL.Path", got, bare)
	}
}

func TestVerifyAcceptsOnlyTheRightSignature(t *testing.T) {
	body := []byte(`{"quantity":"1"}`)
	sig := Sign([]byte(vectorKey), "POST", "/v1/orders", body)

	if !Verify([]byte(vectorKey), "POST", "/v1/orders", body, sig) {
		t.Fatal("Verify rejected the signature Sign produced — the gateway would 401 every " +
			"correctly signed request on the platform")
	}
	// Each of these is a real caller mistake, and each must be refused.
	for name, bad := range map[string]struct {
		key    string
		method string
		path   string
		body   []byte
	}{
		"wrong key":    {"other", "POST", "/v1/orders", body},
		"wrong method": {vectorKey, "PUT", "/v1/orders", body},
		"wrong path":   {vectorKey, "POST", "/v1/order", body},
		"body edited":  {vectorKey, "POST", "/v1/orders", []byte(`{"quantity":"9"}`)},
	} {
		if Verify([]byte(bad.key), bad.method, bad.path, bad.body, sig) {
			t.Errorf("%s: Verify accepted a signature that does not cover the request", name)
		}
	}
}

// TestAnEmptyKeySetsNoHeaderAtAll is the distinction that produced #774's
// misdiagnosis one layer up. A deployment with no signing secret is a legal
// posture — middleware.Signing is a pass-through when its secret is empty — but
// an X-Signature whose VALUE is empty is not the same request as one carrying no
// such header. The middleware reads the first as present-and-wrong and answers
// 401 where it would otherwise have accepted the request unsigned.
func TestAnEmptyKeySetsNoHeaderAtAll(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gw/v1/x", nil)
	SignRequest(req, nil, nil)

	if _, present := req.Header[http.CanonicalHeaderKey(Header)]; present {
		t.Fatalf("%s was set with no signing key: %q. An empty signature is refused; an absent "+
			"one is accepted by an unsigned deployment", Header, req.Header.Get(Header))
	}
}

// TestSignRequestDoesNotConsumeTheBody. The body is passed separately precisely
// so the request's own one-shot reader is left alone; a helper that read it
// would sign correctly and send nothing.
func TestSignRequestDoesNotConsumeTheBody(t *testing.T) {
	body := []byte(`{"quantity":"1"}`)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/orders", bytes.NewReader(body))
	SignRequest(req, []byte(vectorKey), body)

	got := make([]byte, len(body))
	n, _ := req.Body.Read(got)
	if n != len(body) || !bytes.Equal(got[:n], body) {
		t.Fatalf("the request body was consumed by signing: read %d of %d bytes", n, len(body))
	}
}
