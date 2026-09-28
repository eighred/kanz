package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestInviteLifecycleRequiresAuthorityAndReviewedRevision(t *testing.T) {
	for _, action := range []string{"revoke", "reissue"} {
		t.Run(action, func(t *testing.T) {
			path := "/invites/source/" + action
			f := newProvServer(t, operatorClaims(), nil)
			for _, body := range []string{`{}`, `null`, `{"revision":-1}`, `{"revision":0,"roles":["kanz-trader"]}`, `{"revision":0} {}`} {
				r := httptest.NewRequest("POST", path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer tok")
				mux := http.NewServeMux()
				f.s.Routes(mux)
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, r)
				if rr.Code != 400 {
					t.Fatalf("unreviewed command accepted: %d", rr.Code)
				}
			}
			if f.store.inviteChanges != 0 {
				t.Fatal("invalid body reached store")
			}
			body := map[string]int{"revision": 0}
			if rr := f.req(t, "POST", path, "", body, map[string]string{"X-Kanz-Principal-Roles": identity.AdminRole}); rr.Code != 401 {
				t.Fatal("forged principal admitted")
			}
			f.vfy.claims.Roles = []string{"kanz-user"}
			if rr := f.req(t, "POST", path, "tok", body, nil); rr.Code != 403 {
				t.Fatal("nonadministrator admitted")
			}
			f.vfy.claims = operatorClaims()
			f.vfy.claims.SessionEpoch = 99
			if rr := f.req(t, "POST", path, "tok", body, nil); rr.Code != 401 {
				t.Fatal("stale token admitted")
			}
			if f.store.inviteChanges != 0 {
				t.Fatal("unauthorized mutation reached store")
			}
		})
	}
}

func TestInviteLifecycleErrorsAreBoundedAndNeverReturnTokenMaterial(t *testing.T) {
	for _, action := range []string{"revoke", "reissue"} {
		for _, tc := range []struct {
			err  error
			code int
		}{{identity.ErrInviteNotFound, 404}, {identity.ErrInviteConflict, 409}, {identity.ErrAdminAuthority, 401}, {identity.ErrInvitePolicy, 403}, {errors.New("synthetic-token-hash-from-driver"), 503}} {
			f := newProvServer(t, operatorClaims(), nil)
			f.store.err = tc.err
			rr := f.req(t, "POST", "/invites/source/"+action, "tok", map[string]int{"revision": 0}, nil)
			if rr.Code != tc.code || strings.Contains(rr.Body.String(), "synthetic-token") || strings.Contains(rr.Body.String(), "invite_token") {
				t.Fatalf("unsafe error response: code=%d", rr.Code)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("mutation response cacheable")
			}
		}
	}
}
