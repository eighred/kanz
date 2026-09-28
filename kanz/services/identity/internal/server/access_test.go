package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestAccessEndpointsRequireCurrentAdministrator(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	for _, tc := range []struct{ method, path string }{{"GET", "/users"}, {"PUT", "/users/alice/access"}} {
		rr := f.req(t, tc.method, tc.path, "", nil, nil)
		if rr.Code != 401 {
			t.Fatalf("anonymous: %d", rr.Code)
		}
		f.vfy.claims.Roles = []string{"kanz-user"}
		rr = f.req(t, tc.method, tc.path, "tok", nil, nil)
		if rr.Code != 403 {
			t.Fatalf("nonadmin: %d", rr.Code)
		}
		f.vfy.claims = operatorClaims()
	}
}

func TestIdentityPermissionsNeverTrustTokenRolesWithoutCurrentAccount(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	rr := f.req(t, http.MethodGet, "/permissions", "tok", nil, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"identity_admin":true`) {
		t.Fatalf("permissions: %d %s", rr.Code, rr.Body.String())
	}
	f.vfy.err = errors.New("expired")
	rr = f.req(t, http.MethodGet, "/permissions", "tok", nil, nil)
	if rr.Code != 401 {
		t.Fatalf("expired: %d", rr.Code)
	}
}
