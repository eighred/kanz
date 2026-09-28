package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
)

func TestRealHTTPRotationRequiresCredentialAndRevokesAllOldTokens(t *testing.T) {
	st, _ := sessionStorePool(t)
	ctx := context.Background()
	now := time.Now()
	old := "synthetic-original-password"
	replacement := "synthetic-replacement-password"
	hash, err := identity.HashCredential(old)
	if err != nil {
		t.Fatal(err)
	}
	raw, digest, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("rotate", digest, "user:rotate", "acme", []string{"kanz-user"}, nil, "test:admin", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	u, err := st.Redeem(ctx, raw, hash, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, "https://rotation.test", "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	s, err := New(st, signer, ratelimit.New(ratelimit.Options{Burst: 100}), func() any { return signer.JWKS() }, "https://rotation.test", logger, WithProvisioning(Provisioning{Store: st, Verifier: signer, AdminRole: identity.AdminRole, Audit: auth.NewSlogRecorder(logger)}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	token, _, err := signer.Mint(u)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, bearer string, body any, want int) string {
		t.Helper()
		b, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		r, e := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		r.Close = true
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, e := srv.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		payload, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d", path, resp.StatusCode, want)
		}
		for _, secret := range []string{old, replacement, string(hash)} {
			if bytes.Contains(payload, []byte(secret)) {
				t.Fatal("credential material in response")
			}
		}
		return string(payload)
	}
	body := func(current, new string) any {
		return map[string]string{"current_credential": current, "new_credential": new}
	}
	refused := request("/credential", token, body("incorrect", replacement), 401)
	unknown := *u
	unknown.Subject = "missing"
	unknownToken, _, err := signer.Mint(&unknown)
	if err != nil {
		t.Fatal(err)
	}
	if got := request("/credential", unknownToken, body(old, replacement), 401); got != refused {
		t.Fatal("unknown subject distinguished")
	}
	request("/credential", token, body(old, "short"), 400)
	request("/credential", token, body(old, old), 400)
	request("/credential", "", body(old, replacement), 401)
	request("/credential", token, body(old, replacement), 204)
	request("/credential", token, body(replacement, "another-valid-password"), 401)
	request("/login", "", loginRequest{Subject: u.Subject, Credential: old}, 401)
	request("/login", "", loginRequest{Subject: u.Subject, Credential: replacement}, 200)
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionEpoch != 1 {
		t.Fatal("rotation failed to advance epoch")
	}
	if err = st.SetStatus(ctx, identity.Administration{Subject: "test:admin", Tenant: "acme"}, u.Subject, identity.StatusDisabled, time.Now()); err != nil {
		t.Fatal(err)
	}
	fresh, err = st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	// Even a correctly signed token with the current epoch cannot rotate a disabled account.
	fresh.Status = identity.StatusActive
	disabledToken, _, err := signer.Mint(fresh)
	if err != nil {
		t.Fatal(err)
	}
	request("/credential", disabledToken, body(replacement, "another-valid-password"), 401)
	for _, secret := range []string{old, replacement, string(hash)} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential material in logs")
		}
	}
}

func TestRotationSharesLoginRateLimit(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	f.s.limiter = &allowN{n: 0}
	rr := f.req(t, "POST", "/credential", "tok", map[string]string{"current_credential": "guess", "new_credential": "replacement-password"}, nil)
	if rr.Code != 429 {
		t.Fatalf("unbounded credential verification: %d", rr.Code)
	}
}
