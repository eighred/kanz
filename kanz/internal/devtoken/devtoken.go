// Package devtoken mints HS256 bearer tokens for LOCAL DEVELOPMENT against the
// api-gateway's bundled dev validator (API_GATEWAY_JWT_SECRET).
//
// It exists because the gateway no longer starts unauthenticated (SEC-M1): with
// no credential configured it exits 2 rather than serving /v1/* — POST /v1/orders
// included — to anyone. That is the right stance, and it means the dev compose
// stack, the k6 load harness and the in-cluster proof all need a real token. They
// were minting them by hand, or not at all.
//
// The tokens it mints carry a fixed `iss` and `aud` (pkg/auth.DevHS256Issuer /
// DevHS256Audience) that the gateway REQUIRES, so a token-shaped blob signed
// with the same shared secret for some other purpose does not authenticate a
// caller (#242). Both sides read the two values from pkg/auth rather than
// spelling them twice.
//
// This is NOT a production credential path. Production identity is OIDC/JWKS
// against Eighred SSO (pkg/auth), where kanz holds no signing key and mints
// nothing; a shared HMAC secret is a symmetric credential every holder can forge
// with. Nothing here is built into a service image — the tool is a standalone
// CLI, like kanz-halt.
package devtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/eighred/kanz/pkg/auth"
)

// Claims is the identity a dev token asserts. Tenant is the RLS scope every
// downstream query runs under, so it is not optional: a token with no tenant
// authenticates a caller who can be given no data.
type Claims struct {
	Subject string
	Tenant  string
	Roles   []string
	TTL     time.Duration
}

// claims mirrors the gateway's wire shape (services/api-gateway/internal/
// middleware.jwtClaims), which is unexported and unimportable from here. The
// contract is pinned by TestDevTokenAcceptedByGateway, which drives the real
// validator rather than a copy of it.
//
// iss/aud are FIXED, not caller-supplied: the gateway requires exactly these
// two values (#242), and both sides read them from pkg/auth so the minter and
// the validator cannot drift into a dev estate where nothing authenticates.
type claims struct {
	Subject  string   `json:"sub"`
	Tenant   string   `json:"tenant"`
	Roles    []string `json:"roles"`
	Issuer   string   `json:"iss"`
	Audience string   `json:"aud"`
	Expiry   int64    `json:"exp"`
}

// Mint returns a compact JWS (header.payload.signature) the gateway's HS256
// validator accepts.
func Mint(secret string, c Claims) (string, error) {
	if secret == "" {
		return "", errors.New("devtoken: empty secret — a token signed with no secret is forgeable by anyone")
	}
	if c.Subject == "" || c.Tenant == "" {
		return "", errors.New("devtoken: subject and tenant are required (tenant is the RLS scope every query runs under)")
	}
	if c.TTL == 0 {
		c.TTL = time.Hour
	}

	enc := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}

	header, err := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := enc(claims{
		Subject:  c.Subject,
		Tenant:   c.Tenant,
		Roles:    c.Roles,
		Issuer:   auth.DevHS256Issuer,
		Audience: auth.DevHS256Audience,
		Expiry:   time.Now().Add(c.TTL).Unix(),
	})
	if err != nil {
		return "", err
	}

	signed := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
