package server

import "net/http"

const browserCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; manifest-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// Safe OIDC GET redirects remain protected by the existing state/PKCE flow.
// Headerless non-browser clients retain net/http's documented behavior: this
// is a browser forgery boundary, not a replacement for session authentication.
func (s *Server) browserRequestAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if len(r.Header.Values("Origin")) > 1 || len(r.Header.Values("Sec-Fetch-Site")) > 1 {
		return false
	}
	if origins := r.Header.Values("Origin"); len(origins) != 0 {
		// SecureCookies is the configured HTTPS edge contract, even when the
		// trusted tunnel uses HTTP on loopback. Caller-supplied Forwarded and
		// X-Forwarded-* headers must not redefine that contract or our Host.
		scheme := "http"
		if s.secureCookies || r.TLS != nil {
			scheme = "https"
		}
		// Origin is serialized as scheme://authority. Exact equality also
		// rejects opaque origins, userinfo, paths, lists and empty values.
		if origins[0] != scheme+"://"+r.Host {
			return false
		}
	}
	return s.browserOrigin.Check(r) == nil
}

func setBrowserHeaders(h http.Header, secure bool) {
	discardUpstreamBrowserPolicy(h)
	h.Set("Content-Security-Policy", browserCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if secure {
		// Scope to this host; sibling domains have their own transport policy.
		h.Set("Strict-Transport-Security", "max-age=31536000")
	} else {
		h.Del("Strict-Transport-Security")
	}
}

// ReverseProxy copies response headers with Add. Strip upstream policy so the
// browser receives precisely the policy already installed on our writer.
func discardUpstreamBrowserPolicy(h http.Header) {
	for _, name := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only",
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Permissions-Policy",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Strict-Transport-Security",
		"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
		"Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Expose-Headers"} {
		h.Del(name)
	}
}
