package authz_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

func TestEveryPrivilegedCapabilityRequiresRecentMFA(t *testing.T) {
	for _, cap := range []authz.Capability{authz.Read, authz.Trade, authz.Operate, authz.Fund, authz.Approve, authz.Mandate, authz.Audit} {
		for _, tc := range []struct {
			name     string
			required bool
			proof    auth.MFA
			want     bool
		}{{"unenrolled-policy", true, auth.MFA{}, false}, {"legacy-explicit-policy-off", false, auth.MFA{}, true}, {"fresh", true, auth.MFA{Required: true, VerifiedAt: time.Now().Add(-time.Minute)}, true}, {"expired", false, auth.MFA{Required: true, VerifiedAt: time.Now().Add(-auth.StepUpTTL)}, false}, {"future", true, auth.MFA{Required: true, VerifiedAt: time.Now().Add(time.Hour)}, false}} {
			t.Run(string(cap)+"/"+tc.name, func(t *testing.T) {
				rec := &capturingRecorder{}
				m := authz.NewMux(authz.Grants{"user": {cap}}, rec, authz.WithMFARequired(tc.required))
				reached := false
				m.Handle(cap, "POST /protected", func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(204) })
				req := httptest.NewRequest("POST", "/protected", nil)
				req.Header.Set("X-Kanz-MFA", "webauthn-uv")
				req.Header.Set("X-Kanz-MFA-Verified-At", time.Now().Format(time.RFC3339))
				req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "u", Tenant: "tenant", Roles: []string{"user"}, MFA: tc.proof}))
				rr := httptest.NewRecorder()
				m.ServeHTTP(rr, req)
				want := tc.want || cap == authz.Read
				if reached != want || (want && rr.Code != 204) || (!want && rr.Code != 403) {
					t.Fatalf("reached=%v status=%d", reached, rr.Code)
				}
				if !want && !strings.Contains(rr.Body.String(), "mfa_required") {
					t.Fatal("missing actionable step-up refusal")
				}
				d := only(t, rec.entries()).GetAttributes()["decision"]
				if (d == "allow") != want {
					t.Fatal("MFA decision not audited")
				}
			})
		}
	}
}
