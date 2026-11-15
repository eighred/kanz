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
