// Package oidc is the web-BFF's relying-party client for the OIDC
// authorization-code + PKCE flow against Eighred SSO — the browser counterpart
// to pkg/deviceauth's device flow. It resolves the authorize/token endpoints
// from the issuer's discovery document, builds the browser redirect, and
// exchanges the returned code (with the PKCE verifier) for a token. It holds no
// keys and adds no auth endpoints; identity stays owned by Eighred SSO, whose
// JWTs the gateway validates via the delivered OIDCAuthenticator.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config configures the auth-code client.
type Config struct {
	Issuer      string
	ClientID    string
	RedirectURL string
	Scope       string
	HTTPClient  *http.Client
	now         func() time.Time
}

// Client runs the authorization-code + PKCE flow.
type Client struct {
	cfg   Config
	httpc *http.Client
	now   func() time.Time

	authEndpoint  string
	tokenEndpoint string
}

// New validates the config and returns a Client. Discovery is deferred to the
// first use, so construction never blocks on the network.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oidc: client_id is required")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, errors.New("oidc: redirect_url is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Client{cfg: cfg, httpc: cfg.HTTPClient, now: cfg.now}, nil
}

// Token is the credential set the flow yields on a successful code exchange.
type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Expiry       time.Time
}

// PKCE is a generated verifier/challenge pair (RFC 7636, S256). The verifier is
// kept server-side across the redirect and presented at the code exchange; the
// challenge travels in the authorize redirect.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh verifier and its S256 challenge.
func NewPKCE() (PKCE, error) {
	v, err := randomURLSafe(32)
	if err != nil {
		return PKCE{}, err
	}
	sum := sha256.Sum256([]byte(v))
	return PKCE{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// NewState returns a random anti-forgery state value for the redirect.
func NewState() (string, error) { return randomURLSafe(24) }

// AuthCodeURL builds the browser redirect to the SSO authorize endpoint for the
// given state and PKCE challenge. It resolves the endpoint via discovery on
// first use.
func (c *Client) AuthCodeURL(ctx context.Context, state, challenge string) (string, error) {
	if err := c.resolveEndpoints(ctx); err != nil {
		return "", err
	}
	q := url.Values{
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {c.cfg.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {c.cfg.Scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(c.authEndpoint, "?") {
		sep = "&"
	}
	return c.authEndpoint + sep + q.Encode(), nil
}

// tokenResponse mirrors the RFC 6749 token / error response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange redeems the authorization code for a token, presenting the PKCE
// verifier. redirect_uri must match the one sent to authorize.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (*Token, error) {
	if err := c.resolveEndpoints(ctx); err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"redirect_uri":  {c.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("oidc: token endpoint status %d", resp.StatusCode)
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("oidc: decode token response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("oidc: token exchange failed: %s (%s)", out.Error, out.ErrorDescription)
	}
	if out.AccessToken == "" {
		return nil, errors.New("oidc: token response has no access_token")
	}
	return &Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		IDToken:      out.IDToken,
		TokenType:    out.TokenType,
		Expiry:       c.now().Add(time.Duration(out.ExpiresIn) * time.Second),
	}, nil
}

// resolveEndpoints fetches the OIDC discovery document once and caches the
// authorize and token endpoints. The discovered issuer must match the
// configured one.
func (c *Client) resolveEndpoints(ctx context.Context) error {
	if c.authEndpoint != "" && c.tokenEndpoint != "" {
		return nil
	}
	discoURL := strings.TrimRight(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoURL, nil)
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
		return fmt.Errorf("oidc: discovery status %d", resp.StatusCode)
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("oidc: decode discovery: %w", err)
	}
	if doc.Issuer != c.cfg.Issuer {
		return fmt.Errorf("oidc: discovery issuer %q != configured %q", doc.Issuer, c.cfg.Issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return errors.New("oidc: issuer does not advertise authorize/token endpoints")
	}
	c.authEndpoint = doc.AuthorizationEndpoint
	c.tokenEndpoint = doc.TokenEndpoint
	return nil
}

// randomURLSafe returns n random bytes as a base64url (no padding) string.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
