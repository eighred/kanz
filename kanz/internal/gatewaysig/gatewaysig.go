// Package gatewaysig is the ONE statement of the api-gateway's API-01d request
// signature: who computes it, how, and under which header.
//
// # Why it exists
//
// The gateway rejects an unsigned request BEFORE authentication runs whenever
// API_GATEWAY_SIGNING_SECRET_FILE is set, which every deployment manifest does.
// So every component that calls it must produce the identical bytes — and until
// this package, four independent places each stated how:
//
//	internal/tui/gateway.Sign          the client the TUI dials with
//	services/web-bff signRequest       its own reproduction, in its own words
//	infra/onboarding/provision-tenant.sh  openssl + base64 + tr, in shell
//	api-gateway middleware.Signing     the VERIFIER, which is the same fact again
//
// THREE CALLERS SHIPPED WITHOUT IT, one after another: the Copilot REPL's client
// (#198), provision-tenant.sh's isolation probes (#774), and services/web-bff
// (#777). Each was repaired by re-deriving the signature at the call site, which
// is why there was a fourth to find. CLAUDE.md names the shape exactly: "A
// copied helper is how a fix stops spreading."
//
// # Why the VERIFIER lives here too, and not only the signers
//
// services/web-bff's copy carried its own reason for existing: "It is a
// reproduction rather than a shared call because the gateway's verifier lives
// inside that service." That was true and it is the thing this package changes.
// A shared signer that the verifier does not use is a convenience — the two can
// still drift, and the drift surfaces as a 401 nobody can attribute. A shared
// signer the verifier is BUILT FROM is a fact: a caller using Sign is correct by
// construction, because Verify is the same function.
//
// # What a 401 from a missing signature looks like, and why it recurs
//
// It reads as an authentication failure, because it IS one — the middleware
// answers 401 before Auth runs. #774's operator was told the token was bad and
// minted three fresh ones, each reproducing the identical 401; the plausible
// next step is to conclude the isolation gate is broken and hand a tenant over
// without it, which is the one outcome that script exists to prevent. #777's web
// app authenticated perfectly and 401'd on every screen carrying data, because
// /auth/* never crosses the gateway. Neither is visible to any test that does
// not cross the boundary.
package gatewaysig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

// Header is the request header carrying the signature. Every producer and
// consumer names it from here.
//
// IT IS A CONSTANT RATHER THAN A LITERAL because a header name is half of the
// contract and the cheaper half to get wrong: a caller that signs correctly
// under "X-Signature-V2" is refused exactly like one that does not sign at all,
// and the 401 says the same thing. test/arch/gateway_signature_test.go fails the
// build on a literal spelling of it outside this package.
const Header = "X-Signature"

// Sign returns the API-01d signature for one request: HMAC-SHA256 over
//
//	METHOD "\n" PATH "\n" BODY
//
// base64url-encoded WITHOUT padding.
//
// EACH PIECE OF THAT IS LOAD-BEARING and none of it is arbitrary:
//
//   - PATH, not the request URI. Query is excluded, so a caller may not sign a
//     URL with its query attached — the gateway hashes r.URL.Path.
//   - The trailing newline before the body is present even when the body is
//     empty, which is why a GET signs over METHOD "\n" PATH "\n" and nothing
//     more. A shell caller writing printf '%s\n%s' drops it and every signature
//     is wrong by one byte.
//   - RawURLEncoding: base64url with '-' and '_', and NO '=' padding. Standard
//     base64 differs on exactly the inputs whose length is not a multiple of
//     three, so it is right most of the time, which is the worst way to be wrong.
//
// A nil body is the empty body, not a distinct value.
func Sign(key []byte, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(method + "\n" + path + "\n"))
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is the signature for this request, in constant
// time.
//
// hmac.Equal, never ==. A byte-by-byte comparison that returns early leaks how
// much of a forged signature was right, which over enough attempts is the
// signature — and the gateway is the one component on this platform an
// unauthenticated caller can reach.
func Verify(key []byte, method, path string, body []byte, sig string) bool {
	return hmac.Equal([]byte(Sign(key, method, path, body)), []byte(sig))
}

// SignRequest stamps the signature onto an outbound request.
//
// IT TAKES THE BODY SEPARATELY, and that is not an inconvenience to design away.
// An http.Request's Body is a one-shot reader: signing it here would consume the
// bytes the transport is about to send, and a helper that read and replaced the
// body would silently change ContentLength on a caller that had set it. The
// caller already holds the bytes — it had to, to build the request — so passing
// them is honest about who owns them.
//
// AN EMPTY KEY SETS NO HEADER, and the distinction matters. A deployment that
// configures no signing secret is a legal posture (middleware.Signing is a
// pass-through when its secret is empty), and an X-Signature whose VALUE is
// empty is not the same request as one carrying no such header: the gateway
// reads the first as present-and-wrong and answers 401 where it would have
// accepted the request unsigned. provision-tenant.sh carries the same rule in
// shell, for the same reason.
func SignRequest(r *http.Request, key []byte, body []byte) {
	if len(key) == 0 {
		return
	}
	r.Header.Set(Header, Sign(key, r.Method, r.URL.Path, body))
}
