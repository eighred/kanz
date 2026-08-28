// Package gateway is the TUI's one way of talking to the api-gateway.
//
// It carries the transport concerns every TUI surface shares — the bearer token,
// the HMAC request signature, the deadline rule, and the read limit — so that a
// second surface does not arrive with its own copy of them. It deliberately does
// NOT carry a codec or the operator-facing wording for a failure: the control
// plane speaks protojson over the operator's generated types, other surfaces
// speak ordinary JSON DTOs, and a 404 means something different on each of them.
//
// # Why this is a package and not a helper inside one caller
//
// It was a helper inside one caller. internal/tui/universe/gatewaysource.go held
// the token, the signature and the client, because the estate view was the only
// thing that talked to the gateway. A second consumer appeared — the positions,
// PnL and risk surface (#84) — and the rule this repository states for that
// moment is to promote rather than copy.
//
// The fix that must be able to spread is the SIGNATURE. A request signed
// differently from the way the gateway's Signing middleware expects is rejected
// with a 401, and a 401 reads as an expired token: a second copy of Sign that
// drifted by one newline would send an operator to re-authenticate against a
// problem that had nothing to do with their credential.
package gateway

import (
	"bytes"
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/gatewaysig"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Config is what the TUI needs to reach the gateway. None of it is a kubeconfig:
// the TUI is an ordinary API client, and the gateway is the platform's single
// identity authority.
type Config struct {
	// BaseURL of the gateway, e.g. https://api.eighred.com.
	BaseURL string
	// Token returns the bearer credential identifying the HUMAN, read fresh on
	// EVERY request. The gateway decides what it may do; this tool asserts
	// nothing about its own authority.
	//
	// A FUNCTION RATHER THAN A STRING so a re-login mid-session is picked up
	// without rebuilding the client. That shape came from the Copilot REPL's own
	// client, which had it right (#198) — a captured string goes stale the moment
	// somebody runs /login, and the symptom is a 401 that looks like an expired
	// session because it IS one, just not the one they think.
	//
	// Use StaticToken for a caller that genuinely holds a fixed credential.
	Token func() string
	// MaxResponseBytes bounds one response body. Zero ⇒ defaultMaxResponse.
	//
	// Configurable because the surfaces differ by an order of magnitude: a
	// control-plane response is small and a bound near it is a useful sanity
	// check, while a copilot answer with citations is not.
	MaxResponseBytes int64
	// SigningSecret is the shared HMAC key when the deployment sets
	// API_GATEWAY_SIGNING_SECRET. Empty is valid — the gateway's Signing
	// middleware is a no-op when it holds no secret, so a deployment without one
	// needs none here either.
	SigningSecret string
}

// Client performs signed, authenticated requests against the gateway.
// defaultMaxResponse bounds a response body when the caller names no limit. It
// is a guard against a hostile or broken upstream streaming forever, not a
// statement about how big a legitimate answer is.
const defaultMaxResponse = 1 << 20

// StaticToken adapts a fixed credential to Config.Token, for a caller that holds
// one rather than a session that can be renewed.
func StaticToken(tok string) func() string { return func() string { return tok } }

type Client struct {
	base    string
	token   func() string
	signKey []byte
	maxBody int64
	hc      *http.Client
}

// New validates the configuration and builds the client. The errors are written
// for an operator who does not know how the platform is wired.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("no gateway URL: set --gateway-url or KANZ_GATEWAY_URL " +
			"(e.g. https://api.eighred.com). The TUI reaches the estate through the API " +
			"gateway; it does not talk to the cluster directly")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gateway URL %q is not a valid absolute URL (want scheme://host)", cfg.BaseURL)
	}
	// Called once here so a caller with no session still fails at construction
	// with the message below, rather than per-request with a 401 — the token is
	// re-read on every request regardless.
	if cfg.Token == nil || strings.TrimSpace(cfg.Token()) == "" {
		return nil, fmt.Errorf("no token: set --token-file or KANZ_TOKEN. The gateway " +
			"authenticates a PERSON — this tool holds no authority of its own")
	}
	maxBody := cfg.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxResponse
	}
	return &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		signKey: []byte(cfg.SigningSecret),
		maxBody: maxBody,
		// NO Timeout ON THIS CLIENT, ON PURPOSE. Every call site sets an explicit
		// context deadline, and http.Client.Timeout is enforced INDEPENDENTLY of the
		// request context: a client with both bounds is bounded by min(the two), so the
		// smaller silently wins and the layer ordering can no longer be read off the
		// constants that declare it. That is not theoretical — a 30s client timeout sat
		// underneath the 60s the operator needs to run a probe Job, so Test Connection
		// died client-side reporting "Client.Timeout exceeded while awaiting headers"
		// and pointed the operator at the gateway and the network instead of the host
		// they were probing. The per-call context is the single source of truth for how
		// long a call may take; test/arch asserts this literal stays bare.
		hc: &http.Client{},
	}, nil
}

// BaseURL is the gateway this client talks to, for messages that need to name it.
func (c *Client) BaseURL() string { return c.base }

// StatusError is a non-2xx response. Detail is the gateway's own "error" field
// where it sent one, else the trimmed raw body.
//
// It is a TYPE rather than a rendered message because the same status means
// different things on different surfaces: a 404 on /v1/control means the gateway
// was started without an operator address, while a 404 on a broker route means
// something else entirely. Each caller renders it in its own words. Sharing the
// wording here would put the control plane's explanation on every surface, which
// is how an operator ends up chasing a setting that was never involved.
type StatusError struct {
	Status int
	Detail string
	Body   []byte
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("gateway returned %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("gateway returned %d", e.Status)
}

// Do performs one request and returns the response body. query and body may be
// nil.
//
// QUERY IS SEPARATE FROM PATH BECAUSE THE SIGNATURE IS OVER THE PATH ALONE. The
// gateway's Signing middleware computes HMAC over METHOD, then r.URL.Path, then
// the body, newline-separated (services/api-gateway/internal/middleware).
// r.URL.Path EXCLUDES the query string, so a caller that folded "?as_of=..."
// into path would sign more than the gateway does, and get a 401 that reads as
// an expired token. The two cannot be one argument.
//
// A non-2xx yields a *StatusError. A missing context deadline is refused rather
// than performed — see below.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	// The HTTP client deliberately carries no Timeout of its own, so the caller's context is
	// the ONLY thing bounding this request. A call site that forgets a deadline would
	// therefore hang the TUI forever on an unresponsive gateway — the worst failure mode in
	// an operator tool, because it looks like a frozen program rather than an error. Refuse
	// it instead: this is a programming mistake, and it should be loud and immediate the
	// first time it is exercised rather than a hang someone has to bisect.
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("internal: %s %s was called with no context deadline; every "+
			"gateway call must set one (the HTTP client sets no timeout of its own)",
			method, path)
	}

	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Read fresh, so a /login during the session is picked up on the next call.
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// X-API-Version is deliberately NOT sent. The gateway validates it only when
	// present, and the constant lives in an internal/ package this binary cannot
	// import — sending a hardcoded copy would be a second spelling of the version,
	// free to drift into a 406 that looks like an outage.
	gatewaysig.SignRequest(req, c.signKey, body)

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway unreachable at %s: %w", c.base, err)
	}
	defer func() { _ = res.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(res.Body, c.maxBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &StatusError{Status: res.StatusCode, Detail: detailOf(payload), Body: payload}
	}
	return payload, nil
}

// Sign is internal/gatewaysig.Sign, kept under this name because it was the
// tree's only signer and #195 promoted it here for callers that still reach for
// it.
//
// THE IMPLEMENTATION MOVED OUT OF THIS PACKAGE ON PURPOSE (#781). This one lives
// in internal/tui, and internal/tui is the surface #371 retires; when the TUI
// goes, the reference implementation would have gone with it, leaving the
// web-bff and shell copies to drift against a verifier none of them shares. The
// canonicalization now lives beside the verifier that enforces it.
func Sign(key []byte, method, path string, body []byte) string {
	return gatewaysig.Sign(key, method, path, body)
}
