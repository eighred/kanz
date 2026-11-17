//go:build linux && state_read_integration

package grpcsrv_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type readIssuer struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newReadIssuer(t *testing.T) *readIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &readIssuer{key: key}
	issuer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "read-proof-fixture",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case "/revocations":
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "kanz.revocations.v2", "as_of": time.Now().Unix(), "entries": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (i *readIssuer) token(t *testing.T, subject, tenant string, roles, portfolios []string) string {
	t.Helper()
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := encode(map[string]string{"alg": "RS256", "kid": "read-proof-fixture", "typ": "JWT"})
	claims := encode(map[string]any{
		"iss": i.server.URL, "aud": "read-proof", "sub": subject, "tenant": tenant,
		"roles": roles, "portfolios": portfolios, "iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	payload := header + "." + claims
	hash := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}
