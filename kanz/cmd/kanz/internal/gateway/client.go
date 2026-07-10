// Package gateway is the CLI's HTTP client for the api-gateway /v1 edge. It
// talks to the SAME public REST surface any client uses — no mesh mTLS, no
// backend coupling — presenting the Eighred SSO token as a bearer. The gateway
// validates that token via the delivered OIDC/JWKS Authenticator.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the gateway's /v1 endpoints as the authenticated caller.
type Client struct {
	base  string
	httpc *http.Client
	// token returns the current bearer access token. It is a function, not a
	// value, so a re-login mid-session is picked up without rebuilding the
	// client.
	token func() string
}

// New returns a Client for the gateway base URL, reading the bearer from token
// on each request.
func New(baseURL string, token func() string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		httpc: &http.Client{Timeout: 60 * time.Second},
		token: token,
	}
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
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if tok := c.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: extractError(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gateway: decode %s response: %w", path, err)
	}
	return nil
}

// extractError pulls the "error" field out of a gateway error body, falling
// back to the raw text (trimmed) when the body is not the expected shape.
func extractError(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(raw))
}
