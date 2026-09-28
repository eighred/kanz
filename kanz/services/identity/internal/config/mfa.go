package config

import (
	"errors"
	"net/url"
	"os"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

type MFA struct {
	WebAuthn          *webauthn.WebAuthn
	RequirePrivileged bool
}

func loadMFA() (*MFA, error) {
	enabled, required := os.Getenv("IDENTITY_MFA_ENABLED"), os.Getenv("IDENTITY_MFA_REQUIRED")
	for _, value := range []string{enabled, required} {
		if value != "" && value != "true" && value != "false" {
			return nil, errors.New("MFA flags must be true or false")
		}
	}
	if enabled != "true" {
		if required == "true" {
			return nil, errors.New("required MFA needs an enabled provider")
		}
		return nil, nil
	}
	origin, rpid := os.Getenv("IDENTITY_WEBAUTHN_ORIGIN"), os.Getenv("IDENTITY_WEBAUTHN_RP_ID")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || rpid == "" || u.Hostname() != rpid {
		return nil, errors.New("WebAuthn requires an exact HTTPS origin and matching RP hostname")
	}
	wa, err := webauthn.New(&webauthn.Config{RPID: rpid, RPDisplayName: "Kanz", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return nil, errors.New("invalid WebAuthn configuration")
	}
	return &MFA{WebAuthn: wa, RequirePrivileged: required == "true"}, nil
}
