// Package signingkey loads the identity service's token-signing key (#364).
//
// THIS KEY IS THE PLATFORM'S ROOT OF TRUST FOR IDENTITY. Anything holding it can
// mint a token for any subject, in any tenant, with any role — including
// kanz-trader, which places orders. It never leaves this service: the gateway
// verifies with the public half fetched from /jwks.json, which is the whole
// point of slice 3 making issuance asymmetric.
//
// AN EPHEMERAL KEY IS AN EXPLICIT OPT-IN, NOT A FALLBACK. Generating one when
// none is configured would "work" perfectly on a laptop and then, in a
// deployment, silently invalidate every session on every restart and every
// rollout — while the JWKS the gateway cached still advertises the old key, so
// the failure surfaces as intermittent 401s rather than as a missing key. That
// is the shape of misconfiguration this repository refuses: a default that looks
// healthy.
package signingkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/internal/identity"
)

// Load returns the signing key.
//
// path names a PEM file holding an EC private key (SEC1 or PKCS#8). When path is
// empty, allowEphemeral decides between generating a throwaway key — announced
// loudly — and refusing to start.
func Load(path string, allowEphemeral bool, logger *slog.Logger) (*ecdsa.PrivateKey, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(path) == "" {
		if !allowEphemeral {
			return nil, errors.New("identity: no signing key configured. Set IDENTITY_SIGNING_KEY_FILE " +
				"to a PEM holding a P-256 EC private key. For local development set " +
				"IDENTITY_ALLOW_EPHEMERAL_KEY=true instead, which generates a throwaway key and " +
				"invalidates every session on restart — never do that in a deployment")
		}
		key, err := identity.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("identity: generate ephemeral key: %w", err)
		}
		// LOUD, because the consequence is invisible until a restart: every token
		// this process issued stops verifying, and the gateway's cached JWKS still
		// advertises a key nobody signs with.
		logger.Warn("USING AN EPHEMERAL SIGNING KEY (IDENTITY_ALLOW_EPHEMERAL_KEY=true). " +
			"Every token issued by this process becomes invalid when it restarts, and any " +
			"gateway holding the previous JWKS will reject them until it refetches. " +
			"Development only — set IDENTITY_SIGNING_KEY_FILE for anything durable.")
		return key, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read signing key: %w", err)
	}
	key, err := parsePEM(raw)
	if err != nil {
		return nil, fmt.Errorf("identity: %s: %w", path, err)
	}
	// ES256 IS DEFINED OVER P-256 AND ONLY P-256 (RFC 7518 §3.4). A P-384 key
	// would sign happily while the token still advertised alg=ES256, so every
	// verifier would reject it — an outage that looks like a key-distribution
	// problem rather than a curve mismatch.
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("identity: %s: signing key is on curve %s, but ES256 requires P-256",
			path, key.Curve.Params().Name)
	}
	logger.Info("signing key loaded", "path", path)
	return key, nil
}

func parsePEM(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM file")
	}
	// Both encodings are accepted because both are what `openssl ecparam` and
	// `openssl pkcs8` produce, and refusing one would make a correctly generated
	// key look like a corrupt one.
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("PEM block %q is not an EC private key (SEC1 or PKCS#8)", block.Type)
	}
	k, ok := anyKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PEM block %q holds a %T, but an EC private key is required", block.Type, anyKey)
	}
	return k, nil
}
