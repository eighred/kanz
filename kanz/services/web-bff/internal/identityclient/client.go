// Package identityclient calls the platform identity provider (#364) from the
// BFF, so a browser can exchange a credential for a session.
//
// THE CREDENTIAL PASSES THROUGH AND IS NEVER HELD. It arrives on one request,
// goes out on one request, and is not stored, logged or retried — a retry would
// mean keeping it in memory across an interval, and the error text never carries
// it. The token that comes back is stored server-side by the caller, never
// returned to the browser.
package identityclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrRejected is returned when the identity service refuses the credential. It
// is deliberately one error for every refusal the service makes — unknown
// subject, wrong credential, disabled account — because the service already
// collapses those for the same reason: a caller who can tell them apart can
// enumerate the platform's users.
var ErrRejected = errors.New("identityclient: credential rejected")

// ErrThrottled is returned when the identity service is rate-limiting. It is
// kept DISTINCT from ErrRejected so the browser can be told to wait rather than
// that its password was wrong — the caller may well hold a correct one.
var ErrThrottled = errors.New("identityclient: too many attempts")

// Token is a minted credential and the identity it carries.
type Token struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires_at"`
	Subject string    `json:"subject"`
	Tenant  string    `json:"tenant"`
}

// Client talks to the identity service.
type Client struct {
	base string
	http *http.Client
	// forwardHeader carries the ORIGINAL caller's address to the identity
	// service. Without it every login the BFF forwards arrives from the BFF, so
	// the identity service's limiter keys them all together and one user's failed
	// attempts throttle everybody — while an attacker's attempts hide in the same
	// bucket as legitimate traffic.
	forwardHeader string
}

// New builds a client. forwardHeader is the header the identity service reads
// the caller's address from; empty means the address is not forwarded.
func New(baseURL, forwardHeader string, timeout time.Duration) *Client {
	if timeout <= 0 {
		// Argon2id verification is deliberately slow, and the identity service
		// pays that cost even for an unknown subject (its timing defence). A
		// timeout tighter than that turns every login into a 504.
		timeout = 15 * time.Second
	}
	return &Client{
		base:          strings.TrimRight(baseURL, "/"),
		http:          &http.Client{Timeout: timeout},
		forwardHeader: forwardHeader,
	}
}

// Login exchanges a credential for a token, attributing the attempt to clientIP.
func (c *Client) Login(ctx context.Context, subject, credential, clientIP string) (*Token, error) {
	return c.post(ctx, "/login", map[string]string{
		"subject":    subject,
		"credential": credential,
	}, clientIP)
}

// Redeem exchanges an invite token and a chosen credential for an account, and
// returns a token for it — so an invitee is signed in by the act of accepting,
// rather than being asked for the credential they have just set.
func (c *Client) Redeem(ctx context.Context, inviteToken, credential, clientIP string) (*Token, error) {
	return c.post(ctx, "/invites/redeem", map[string]string{
		"token":      inviteToken,
		"credential": credential,
	}, clientIP)
}

func (c *Client) post(ctx context.Context, path string, body map[string]string, clientIP string) (*Token, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.forwardHeader != "" && clientIP != "" {
		req.Header.Set(c.forwardHeader, clientIP)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identityclient: %s: %w", path, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusBadRequest:
		return nil, ErrRejected
	case http.StatusTooManyRequests:
		return nil, ErrThrottled
	default:
		// The body is NOT included: it is the identity service's message to
		// itself, and forwarding it to a browser is how an internal detail
		// becomes an oracle.
		return nil, fmt.Errorf("identityclient: %s: unexpected status %d", path, resp.StatusCode)
	}

	var tok Token
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("identityclient: decode: %w", err)
	}
	if tok.Token == "" {
		return nil, errors.New("identityclient: identity service returned no token")
	}
	return &tok, nil
}
