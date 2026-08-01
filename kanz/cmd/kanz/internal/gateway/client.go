// Package gateway is the CLI's typed client for the api-gateway /v1 edge. It
// talks to the SAME public REST surface any client uses — no mesh mTLS, no
// backend coupling — presenting the Eighred SSO token as a bearer. The gateway
// validates that token via the delivered OIDC/JWKS Authenticator.
//
// # It owns the ROUTES, not the transport (#198)
//
// The transport — auth header, HMAC request signature, deadline rule, read limit
// — is internal/tui/gateway, shared with every other surface in this binary.
// This package is the /v1 route set and the shapes those routes return.
//
// It used to own its own transport, and that cost two defects:
//
//   - IT DID NOT SIGN REQUESTS. On any deployment setting
//     API_GATEWAY_SIGNING_SECRET, the gateway's Signing middleware rejected every
//     REPL call with a 401 — indistinguishable from an expired session, so the
//     operator would be sent to /login, which could not fix it. The estate panes
//     beside it kept working, because they signed.
//   - IT CARRIED http.Client{Timeout: 60s}. A client timeout is enforced
//     INDEPENDENTLY of the request context, so the effective bound is min(the
//     two) and the smaller wins silently — the exact pattern test/arch forbids on
//     the TUI client, having already watched it misreport a healthy host as a
//     gateway problem.
//
// Both are gone by construction now rather than by being fixed here: there is one
// transport, and it is the one already guarded.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tuigateway "github.com/eighred/kanz/internal/tui/gateway"
)

// Client calls the gateway's /v1 endpoints as the authenticated caller.
type Client struct {
	// build is retried until it succeeds, because THE REPL CONSTRUCTS THIS BEFORE
	// ANYONE HAS SIGNED IN. That is not incidental — it is why the bearer is a
	// function rather than a string. The shared client validates the token at
	// construction, so a client built once at startup would be permanently
	// broken: /login could not repair it, and every command would report "no
	// token" forever. Same lazy-with-retry stance the estate and Book panes take.
	mu    sync.Mutex
	built *tuigateway.Client
	build func() (*tuigateway.Client, error)
}

// client returns the transport, constructing it on first successful use.
func (c *Client) client() (*tuigateway.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.built != nil {
		return c.built, nil
	}
	built, err := c.build()
	if err != nil {
		return nil, err
	}
	c.built = built
	return built, nil
}

// New returns a Client for the gateway base URL, reading the bearer from token
// on EVERY request — so a /login mid-session is picked up without rebuilding.
//
// signingSecret must match the gateway's API_GATEWAY_SIGNING_SECRET where one is
// set; empty is valid and means the gateway enforces no signature.
func New(baseURL string, token func() string, signingSecret string) *Client {
	return &Client{build: func() (*tuigateway.Client, error) {
		return tuigateway.New(tuigateway.Config{
			BaseURL: strings.TrimRight(baseURL, "/"),
			Token:   token,
			// Deployment concern, not a session one: the middleware is a no-op
			// when the gateway holds no secret, so an unset value here is valid.
			SigningSecret: signingSecret,
			// A copilot answer with citations is not a control-plane response;
			// the shared default (1 MiB) would truncate one. This is the limit
			// this client already used, kept rather than silently tightened.
			MaxResponseBytes: 8 << 20,
		})
	}}
}

// AskResult is the copilot's governed answer (POST /v1/ask). The fields mirror
// the copilot server's JSON response exactly.
type AskResult struct {
	Answer           string   `json:"answer"`
	Citations        []string `json:"citations"`
	Grounded         bool     `json:"grounded"`
	Refused          bool     `json:"refused"`
	InjectionFlagged bool     `json:"injection_flagged"`
}

// Ask puts a natural-language question to the copilot.
func (c *Client) Ask(ctx context.Context, question string) (*AskResult, error) {
	body, _ := json.Marshal(map[string]string{"question": question})
	var out AskResult
	if err := c.do(ctx, http.MethodPost, "/v1/ask", nil, bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Exposure fetches a portfolio's exposure. asOf is an optional RFC3339 instant;
// empty ⇒ latest. The response is the gateway's protojson, returned verbatim
// for the caller to render.
func (c *Client) Exposure(ctx context.Context, portfolioID, asOf string) (json.RawMessage, error) {
	q := url.Values{}
	if asOf != "" {
		q.Set("as_of", asOf)
	}
	return c.getRaw(ctx, "/v1/portfolios/"+url.PathEscape(portfolioID)+"/exposure", q)
}

// Measures fetches named risk measures for a portfolio (repeatable ?measure=).
func (c *Client) Measures(ctx context.Context, portfolioID string, measures []string, asOf string) (json.RawMessage, error) {
	q := url.Values{}
	if asOf != "" {
		q.Set("as_of", asOf)
	}
	for _, m := range measures {
		q.Add("measure", m)
	}
	return c.getRaw(ctx, "/v1/portfolios/"+url.PathEscape(portfolioID)+"/measures", q)
}

// Scenario evaluates a scenario against a portfolio. body is the raw
// EvaluateScenarioRequest JSON; the gateway overrides portfolio_id from the
// path, so the caller need not repeat it.
func (c *Client) Scenario(ctx context.Context, portfolioID string, body json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/v1/portfolios/"+url.PathEscape(portfolioID)+"/scenario", nil, bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) getRaw(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// APIError is a non-2xx response from the gateway; it carries the HTTP status
// and the server's error message (the gateway returns {"error": "..."}).
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("gateway returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// do issues the request with the bearer token and decodes a 2xx JSON body into
// out (a *json.RawMessage keeps the body verbatim). A non-2xx becomes an
// *APIError carrying the server's error message.
// callTimeout bounds ONE gateway call.
//
// It replaces the http.Client{Timeout: 60s} this package used to carry, at the
// same 60 seconds — but as a CONTEXT deadline rather than a client timeout, and
// the difference is the whole point of #198. A client timeout is enforced
// independently of the request context, so the effective bound is min(the two)
// and the smaller wins invisibly. A derived context NESTS: a caller that already
// holds an earlier deadline keeps it, and the ordering can be read off the call
// stack instead of being split across two mechanisms.
//
// The REPL hands down the process-lifetime context, which carries no deadline at
// all — so without this every call would be unbounded, which is worse than the
// timeout it replaces. The shared client refuses a deadline-less call outright,
// which is how that was caught rather than shipped.
const callTimeout = 60 * time.Second

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, out any) error {
	transport, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var payload []byte
	if body != nil {
		p, rerr := io.ReadAll(body)
		if rerr != nil {
			return fmt.Errorf("gateway: read request body: %w", rerr)
		}
		payload = p
	}

	// q is passed SEPARATELY rather than folded into path: the gateway signs
	// METHOD, r.URL.Path and the body, and r.URL.Path excludes the query. Signing
	// a path with the query attached produces a 401 that reads as a bad token.
	raw, err := transport.Do(ctx, method, path, q, payload)
	if err != nil {
		// Re-shaped into this package's APIError so the REPL's error handling is
		// unchanged. The status is what callers switch on; the shared client's
		// StatusError exists precisely so each surface words it for itself.
		var se *tuigateway.StatusError
		if errors.As(err, &se) {
			return &APIError{Status: se.Status, Message: se.Detail}
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gateway: decode %s response: %w", path, err)
	}
	return nil
}

// The gateway's error-body shape is parsed once, in internal/tui/gateway — this
// package used to carry its own copy (extractError) and it went with the
// transport. Two parsers for one wire shape is how the two come to disagree
// about what an error says.
