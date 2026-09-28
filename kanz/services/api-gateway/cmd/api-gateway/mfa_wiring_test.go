package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// Exercise both production composition roots with real signed tokens and HTTP
// JWKS/revocation boundaries. A mux-only test cannot detect omitted MFA wiring.
func TestBuildRouterEnforcesSignedMFAAssurance(t *testing.T) {
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	authn, revs := buildWired(t, st, &captureHandler{})
	if err := revs.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, required := range []bool{false, true} {
		cfg := config.Config{RequiredRole: baselineRole, TradeRole: traderRole, ApproveRole: approverRole, MFARequired: required}
		srv := httptest.NewServer(preAuthRouter(t, cfg, authn))
		for _, enrolled := range []bool{false, true} {
			u := &identity.User{Subject: "mfa-approver", Tenant: "acme", Roles: []string{baselineRole, approverRole}, Status: identity.StatusActive}
			if enrolled {
				u.MFA = auth.MFA{Required: true, VerifiedAt: time.Now().UTC()}
			}
			token, _, err := st.signer.Mint(u)
			if err != nil {
				t.Fatal(err)
			}
			r, err := http.NewRequest(http.MethodPost, srv.URL+approvePath, strings.NewReader(approveBody))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Kanz-MFA", "webauthn-uv") // Cannot upgrade the signed proof.
			resp, err := srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			want := http.StatusAccepted
			if required && !enrolled {
				want = http.StatusForbidden
			}
			if resp.StatusCode != want {
				t.Fatalf("required=%v enrolled=%v status=%d want=%d", required, enrolled, resp.StatusCode, want)
			}
		}
		srv.Close()
	}
}
