package authz_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api_gateway/internal/authz"
	"github.com/eighred/kanz/services/api_gateway/internal/middleware"
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

func TestCashHistoryMFAAndTransportUseTheRecordedGrant(t *testing.T) {
	for _, tc := range []struct {
		name, role           string
		private, fresh, want bool
	}{
		{"workload-public", "history", false, false, false}, {"workload-private", "history", true, false, true},
		{"human-stale", "fund", false, false, false}, {"human-fresh", "fund", false, true, true},
		{"human-private", "fund", true, true, false}, {"reader", "reader", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &capturingRecorder{}
			mux := authz.NewMux(authz.Grants{"history": {authz.CashHistory}, "fund": {authz.CashHistory, authz.Fund}, "reader": {authz.Read}}, rec, authz.WithCashHistoryRole("history"), authz.WithMFARequired(true))
			mux.Handle(authz.CashHistory, "GET /cash", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
			p := &middleware.Principal{Subject: "subject", Tenant: "tenant", Roles: []string{tc.role}}
			if tc.fresh {
				p.MFA = auth.MFA{Required: true, VerifiedAt: time.Now().Add(-time.Minute)}
			}
			req := httptest.NewRequest(http.MethodGet, "/cash", nil)
			req = req.WithContext(middleware.WithPrincipal(req.Context(), p))
			w := httptest.NewRecorder()
			var route http.Handler = mux
			if tc.private {
				route = authz.FromOMSHistoryListener(mux)
			}
			route.ServeHTTP(w, req)
			if tc.want && w.Code != http.StatusNoContent || !tc.want && w.Code != http.StatusForbidden {
				t.Fatal("unexpected financial history authorization", w.Code)
			}
			if (only(t, rec.entries()).GetAttributes()["decision"] == "allow") != tc.want {
				t.Fatal("incorrect audit decision")
			}
		})
	}
}
