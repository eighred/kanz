// Package deviceauth is the kanz relying-party client for the RFC 8628
// OAuth 2.0 Device Authorization Grant against the standalone Eighred SSO
// authorization server.
//
// It is the client-side counterpart to pkg/auth's server-side OIDC validator:
// where OIDCAuthenticator (used by the api-gateway edge) verifies an
// Eighred-SSO-issued JWT via JWKS, this package obtains one. Identity is owned
// by Eighred SSO — this package adds NO auth endpoints and holds NO signing
// keys; it only drives the standard device flow and returns the issued token
// for a caller (the kanz CLI, CLI-02) to present as a bearer to the gateway.
//
// The flow (RFC 8628):
//
//  1. POST {device_authorization_endpoint}  → device_code + user_code + URIs.
//  2. The user opens the verification URI and approves the user_code.
//  3. Poll POST {token_endpoint} (device_code grant) until approval, honoring
//     the server's interval and any slow_down back-off, then return the token.
//
// Endpoints are resolved from the issuer's OIDC discovery document, so the
// package is bound to Eighred SSO only by its issuer URL — the same seam the
// gateway's Authenticator uses.
package deviceauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// These sentinels let a caller distinguish the terminal outcomes of the flow
// from a transient/transport error and render an appropriate message.
var (
	// ErrAccessDenied is returned when the user rejects the device at the
	// verification page (RFC 8628 access_denied).
	ErrAccessDenied = errors.New("deviceauth: the request was denied")
	// ErrExpired is returned when the device_code expires before the user
	// approves it (RFC 8628 expired_token).
	ErrExpired = errors.New("deviceauth: the device code expired before approval")
)

// Config configures the device-authorization relying-party client.
type Config struct {
	// Issuer is the Eighred SSO issuer base URL (e.g. https://login.eighred.com).
	// Endpoints are discovered from {Issuer}/.well-known/openid-configuration;
	// the discovered issuer must match this value exactly.
	Issuer string
	// ClientID is the client identifier the kanz CLI is registered as with
	// Eighred SSO. Required.
	ClientID string
	// Scope is the space-delimited scope requested for the token. Optional.
	Scope string
	// HTTPClient fetches discovery and drives the flow (default: a 30s client).
	// A generous default: the token poll below bounds its own timing, but a
	// single request should tolerate a slow network hop to the IdP.
	HTTPClient *http.Client
	// now overrides the clock in tests; nil ⇒ time.Now.
	now func() time.Time
	// after overrides the poll wait in tests; nil ⇒ time.After.
	after func(time.Duration) <-chan time.Time
}

// Client is an RFC 8628 device-authorization relying party. It is safe for
// sequential use by a single CLI process; it is not intended for concurrent
// authorizations (a terminal drives one login at a time).
type Client struct {
	cfg   Config
	httpc *http.Client
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	// endpoints resolved lazily from discovery on first use and cached.
	deviceEndpoint string
	tokenEndpoint  string
}

// New validates the config and returns a Client. Discovery is deferred to the
// first Authorize call, so constructing a Client never blocks on the network.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("deviceauth: issuer is required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("deviceauth: client_id is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.after == nil {
		cfg.after = time.After
	}
	return &Client{cfg: cfg, httpc: cfg.HTTPClient, now: cfg.now, after: cfg.after}, nil
}

// Prompt is the user-facing instruction returned when authorization begins:
// the user visits VerificationURI (or the pre-filled VerificationURIComplete)
// and confirms UserCode before the token can be issued.
type Prompt struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	// ExpiresAt is when the device_code stops being pollable.
	ExpiresAt time.Time
}

// Token is the credential set the flow yields on approval.
type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	// Expiry is the access-token expiry, derived from the response expires_in.
	Expiry time.Time
}

// Authorize runs the full device flow end to end: it begins authorization,
// invokes show exactly once with the verification Prompt so the caller can
// display the user_code and URL, then polls the token endpoint — honoring the
// server-advertised interval and any slow_down back-off — until the user
// approves (returns the Token), denies (ErrAccessDenied), the code expires
// (ErrExpired), or ctx is cancelled.
//
// show must not block indefinitely; it is called synchronously before polling
// starts. A nil show is valid (the caller relies on out-of-band display).
func (c *Client) Authorize(ctx context.Context, show func(Prompt)) (*Token, error) {
	if err := c.resolveEndpoints(ctx); err != nil {
		return nil, err
	}
	auth, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	if show != nil {
		show(Prompt{
			UserCode:                auth.UserCode,
			VerificationURI:         auth.VerificationURI,
			VerificationURIComplete: auth.VerificationURIComplete,
			ExpiresAt:               c.now().Add(time.Duration(auth.ExpiresIn) * time.Second),
		})
	}
	return c.poll(ctx, auth)
}

// deviceAuthResponse mirrors the RFC 8628 §3.2 device-authorization response.
type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// begin performs step 1: request a device_code + user_code pair.
func (c *Client) begin(ctx context.Context) (*deviceAuthResponse, error) {
	form := url.Values{"client_id": {c.cfg.ClientID}}
	if c.cfg.Scope != "" {
		form.Set("scope", c.cfg.Scope)
	}
	var out deviceAuthResponse
	if _, err := c.postForm(ctx, c.deviceEndpoint, form, &out); err != nil {
		return nil, fmt.Errorf("deviceauth: begin authorization: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, errors.New("deviceauth: authorization response missing device_code/user_code")
	}
	return &out, nil
}

// tokenResponse mirrors the RFC 6749 §5.1 token response plus the §5.2 error
// body; a device-grant poll returns one or the other.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// poll performs steps 3+: poll the token endpoint at the advertised interval
// until a terminal outcome. Per RFC 8628 §3.5, authorization_pending means keep
// waiting, slow_down means add 5s to the interval, and access_denied/
// expired_token are terminal.
func (c *Client) poll(ctx context.Context, auth *deviceAuthResponse) (*Token, error) {
	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second // RFC 8628 §3.5 default when unspecified.
	}
	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {auth.DeviceCode},
		"client_id":   {c.cfg.ClientID},
	}
	// The device_code has a hard server-side expiry; stop polling once past it
	// even if the server would keep answering, so a cancelled/abandoned login
	// doesn't spin forever.
	deadline := c.now().Add(time.Duration(auth.ExpiresIn) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.after(interval):
		}
		if auth.ExpiresIn > 0 && c.now().After(deadline) {
			return nil, ErrExpired
		}

		var out tokenResponse
		status, err := c.postForm(ctx, c.tokenEndpoint, form, &out)
		if err != nil {
			return nil, fmt.Errorf("deviceauth: poll token: %w", err)
		}
		if status == http.StatusOK && out.Error == "" {
			return &Token{
				AccessToken:  out.AccessToken,
				RefreshToken: out.RefreshToken,
				IDToken:      out.IDToken,
				TokenType:    out.TokenType,
				Expiry:       c.now().Add(time.Duration(out.ExpiresIn) * time.Second),
			}, nil
		}

		switch out.Error {
		case "authorization_pending":
			// Not approved yet — keep the current cadence.
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrExpired
		default:
			// invalid_grant, invalid_client, or any unexpected error is terminal.
			return nil, fmt.Errorf("deviceauth: token endpoint error %q: %s", out.Error, out.ErrorDescription)
		}
	}
}

// resolveEndpoints fetches the OIDC discovery document once and caches the
// device-authorization and token endpoints. The discovered issuer must match
// the configured one — a mismatch means we resolved endpoints for a different
// issuer than the token will be validated against.
func (c *Client) resolveEndpoints(ctx context.Context) error {
	if c.deviceEndpoint != "" && c.tokenEndpoint != "" {
		return nil
	}
	discoURL := strings.TrimRight(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var doc struct {
		Issuer                      string `json:"issuer"`
		TokenEndpoint               string `json:"token_endpoint"`
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if err := c.getJSON(ctx, discoURL, &doc); err != nil {
		return fmt.Errorf("deviceauth: discover endpoints: %w", err)
	}
	if doc.Issuer != c.cfg.Issuer {
		return fmt.Errorf("deviceauth: discovery issuer %q != configured %q", doc.Issuer, c.cfg.Issuer)
	}
	if doc.DeviceAuthorizationEndpoint == "" {
		return errors.New("deviceauth: issuer does not advertise a device_authorization_endpoint")
	}
	if doc.TokenEndpoint == "" {
		return errors.New("deviceauth: issuer does not advertise a token_endpoint")
	}
	c.deviceEndpoint = doc.DeviceAuthorizationEndpoint
	c.tokenEndpoint = doc.TokenEndpoint
	return nil
}

// postForm posts a URL-encoded form and decodes a JSON body into dst. It
// returns the HTTP status so the caller can distinguish a 200 token response
// from a 4xx typed OAuth error, both of which carry a JSON body. A non-JSON or
// unreachable response is an error; a decoded 4xx error body is not (the caller
// inspects dst.Error).
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// The device and token endpoints return their typed errors as JSON with a
	// 4xx status, so 4xx bodies are still decoded. A 5xx (or any other) status
	// is a real server fault with no contract on the body — surface it.
	if resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: decode response: %w", endpoint, err)
	}
	return resp.StatusCode, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
