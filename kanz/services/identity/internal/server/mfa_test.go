package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/fxamacker/cbor/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/argon2"
)

func TestMFARehashStillRequiresCeremonyAndMissingProviderFailsClosed(t *testing.T) {
	st, pool := sessionStorePool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const password = "synthetic-mfa-legacy-password"
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 8*1024, 1, 32)
	encode := base64.RawStdEncoding.EncodeToString
	weak := identity.Hash(fmt.Sprintf("$argon2id$v=%d$m=8192,t=1,p=1$%s$%s", argon2.Version, encode(salt), encode(key)))
	if !identity.NeedsRehash(weak) || identity.Verify(weak, password) != nil {
		t.Fatal("invalid legacy fixture")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash,status,mfa_enabled) VALUES('mfa-http','acme',ARRAY['kanz-user'],$1,'active',TRUE)`, string(weak)); err != nil {
		t.Fatal(err)
	}
	// BeginLogin uses the public credential descriptor. Actual signature and
	// registration verification are covered by the cryptographic domain/browser tests.
	signingFixture, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	public, e := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: signingFixture.X.FillBytes(make([]byte, 32)), -3: signingFixture.Y.FillBytes(make([]byte, 32))})
	if e != nil {
		t.Fatal(e)
	}
	c := webauthn.Credential{ID: []byte("synthetic-public-credential-id"), PublicKey: public}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identity_mfa_credentials(credential_id,subject,name,credential,created_at) VALUES($3,'mfa-http','public descriptor',$1,$2)`, data, now, base64.RawURLEncoding.EncodeToString(c.ID)); err != nil {
		t.Fatal(err)
	}
	signingKey, _ := identity.GenerateKey()
	signer, _ := identity.NewSigner(signingKey, "https://mfa.test", "kanz-api", time.Hour)
	wa, err := webauthn.New(&webauthn.Config{RPID: "mfa.test", RPOrigins: []string{"https://mfa.test"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithProvisioning(Provisioning{Store: st, Verifier: signer, AdminRole: identity.AdminRole, Audit: auth.NewSlogRecorder(quiet())})}
	srv, err := New(st, signer, ratelimit.New(ratelimit.Options{Burst: 100}), func() any { return signer.JWKS() }, "https://mfa.test", quiet(), append(opts, WithMFA(MFAConfig{Store: st, WebAuthn: wa}))...)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	mfaMux := mux
	body, _ := json.Marshal(loginRequest{Subject: "mfa-http", Credential: password})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/login", bytes.NewReader(body)))
	if w.Code != 202 || bytes.Contains(w.Body.Bytes(), []byte(`"token"`)) {
		t.Fatalf("rehash failed to yield MFA-only challenge: %d", w.Code)
	}
	u, err := st.UserBySubject(ctx, "mfa-http")
	if err != nil || identity.NeedsRehash(u.Credential) || !u.MFA.Required {
		t.Fatal("rehash did not preserve MFA", err)
	}
	disabled, err := New(st, signer, ratelimit.New(ratelimit.Options{Burst: 100}), func() any { return signer.JWKS() }, "https://mfa.test", quiet(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	mux = http.NewServeMux()
	disabled.Routes(mux)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/login", bytes.NewReader(body)))
	if w.Code != 503 || bytes.Contains(w.Body.Bytes(), []byte(`"token"`)) {
		t.Fatal("provider omission bypassed enrolled MFA")
	}
	// Principal headers cannot manufacture a bearer for enrollment.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mfa/register/begin", bytes.NewReader([]byte(`{"name":"bad","password":"unused"}`)))
	r.Header.Set("X-Kanz-Principal-Subject", "mfa-http")
	mfaMux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("principal headers bypassed bearer verification")
	}
}
