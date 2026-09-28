package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
)

func TestHTTPRecoveryRequiresVerifiedMailboxAndRevokesOldSession(t *testing.T) {
	store, _ := sessionStorePool(t)
	ctx := context.Background()
	now := time.Now()
	hash, err := identity.HashCredential("synthetic-old-password")
	if err != nil {
		t.Fatal(err)
	}
	raw, digest, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("recover", digest, "user:recover", "acme", []string{"kanz-user"}, nil, "test:admin", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	u, err := store.Redeem(ctx, raw, hash, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, "https://recovery.test", "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(store, signer, ratelimit.New(ratelimit.Options{Burst: 100}), func() any { return signer.JWKS() }, "https://recovery.test", quiet(), WithProvisioning(Provisioning{Store: store, Verifier: signer, AdminRole: identity.AdminRole, Audit: auth.NewSlogRecorder(quiet())}), WithRecovery(store))
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
	request := func(path, bearer string, body any, want int) {
		t.Helper()
		data, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		r, e := http.NewRequest("POST", srv.URL+path, bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("X-Kanz-Principal-Subject", u.Subject)
		resp, e := srv.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s got %d want %d", path, resp.StatusCode, want)
		}
		if path != "/login" && resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("recovery response cacheable")
		}
	}
	request("/recovery", "", map[string]string{"subject": "unknown"}, 202)
	request("/recovery", "", map[string]string{"subject": u.Subject}, 202)
	if m, e := store.ClaimMail(ctx, now); e != nil || m != nil {
		t.Fatal("mail sent without verified mailbox", e)
	}
	body := map[string]string{"address": "person@example.test", "credential": "synthetic-old-password"}
	request("/mailbox", "forged", body, 401)
	request("/mailbox", token, map[string]string{"address": "person@example.test", "credential": "wrong"}, 401)
	request("/mailbox", token, body, 202)
	request("/mailbox", token, body, 429)
	m, err := store.ClaimMail(ctx, time.Now())
	if err != nil || m == nil {
		t.Fatal("missing verification", err)
	}
	request("/recovery/consume", "", map[string]string{"token": m.Token, "credential": "synthetic-new-password"}, 401)
	request("/mailbox/verify", "", map[string]string{"token": m.Token}, 204)
	request("/mailbox/verify", "", map[string]string{"token": m.Token}, 401)
	request("/recovery", "", map[string]string{"subject": u.Subject, "address": "attacker@example.test"}, 400)
	request("/recovery", "", map[string]string{"subject": u.Subject}, 202)
	m, err = store.ClaimMail(ctx, time.Now())
	if err != nil || m == nil {
		t.Fatal("missing recovery", err)
	}
	request("/recovery/consume", "", map[string]string{"token": m.Token, "credential": "synthetic-new-password"}, 204)
	request("/recovery/consume", "", map[string]string{"token": m.Token, "credential": "synthetic-other-password"}, 401)
	request("/mailbox", token, body, 401)
	request("/login", "", map[string]string{"subject": u.Subject, "credential": "synthetic-old-password"}, 401)
	// Login's existing response has separate cache semantics; check success directly.
	fresh, err := store.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionEpoch != 1 || identity.Verify(fresh.Credential, "synthetic-new-password") != nil {
		t.Fatal("reset did not commit")
	}
}
