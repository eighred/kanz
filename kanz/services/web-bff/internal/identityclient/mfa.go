package identityclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/eighred/kanz/internal/identity"
	"github.com/go-webauthn/webauthn/protocol"
)

// ProjectMFACeremony drops every field outside the browser WebAuthn contract.
// Never relay an opaque upstream body alongside a public ceremony.
func ProjectMFACeremony(raw []byte, registration bool) (*identity.MFACeremony, error) {
	var wire struct {
		ID      string          `json:"id"`
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.ID) != 43 {
		return nil, errors.New("invalid MFA ceremony")
	}
	out := &identity.MFACeremony{ID: wire.ID}
	if registration {
		var c protocol.CredentialCreation
		if err := json.Unmarshal(wire.Options, &c); err != nil || len(c.Response.Challenge) < 16 {
			return nil, errors.New("invalid MFA options")
		}
		c.Response.Extensions = protocol.AuthenticationExtensions{}
		out.Options = c
	} else {
		var c protocol.CredentialAssertion
		if err := json.Unmarshal(wire.Options, &c); err != nil || len(c.Response.Challenge) < 16 {
			return nil, errors.New("invalid MFA options")
		}
		c.Response.Extensions = protocol.AuthenticationExtensions{}
		out.Options = c
	}
	return out, nil
}
func (c *Client) MFA(ctx context.Context, method, path, bearer string, body []byte, clientIP string) (*ProvisioningResponse, error) {
	if method == http.MethodGet && path == "/mfa" {
		return c.authenticated(ctx, method, path, bearer, nil, clientIP)
	}
	if method != http.MethodPost {
		return nil, errors.New("unsupported MFA command")
	}
	switch path {
	case "/mfa/register/begin", "/mfa/register/finish", "/mfa/login/finish", "/mfa/stepup/begin", "/mfa/stepup/finish", "/mfa/remove":
		return c.authenticated(ctx, method, path, bearer, body, clientIP)
	}
	return nil, errors.New("unsupported MFA command")
}
