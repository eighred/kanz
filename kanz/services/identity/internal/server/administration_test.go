package server

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

func TestAdministrationRechecksStoredAuthority(t *testing.T) {
	for _, change := range []string{"tenant", "role", "mixed authority", "revoked token"} {
		t.Run(change, func(t *testing.T) {
			claims := operatorClaims()
			claims.IssuedAt = provNow
			f := newProvServer(t, claims, nil)
			u := f.store.accounts[claims.Subject]
			switch change {
			case "tenant":
				u.Tenant = "another-tenant"
			case "role":
				u.Roles = []string{"kanz-operator"}
			case "mixed authority":
				u.Roles = []string{identity.AdminRole, "kanz-operator"}
			case "revoked token":
				at := provNow.Add(time.Second)
				u.TokensInvalidBefore = &at
			}
			for _, path := range []string{"/invites", "/users/someone/disable", "/users/someone/enable"} {
				rec := f.req(t, http.MethodPost, path, "tok", validBody(), nil)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
			}
			if len(f.store.created) != 0 || len(f.store.statusWrites) != 0 {
				t.Fatal("rejected authority mutated accounts")
			}
		})
	}
}

func TestAdministrationStatusRefusalDoesNotEmitSuccessAudit(t *testing.T) {
	for _, refused := range []error{identity.ErrSelfDisable, identity.ErrLastAdmin, identity.ErrAdminAuthority} {
		t.Run(refused.Error(), func(t *testing.T) {
			f := newStatusServer(t, operatorClaims())
			f.accounts["user:bob"] = account("user:bob", "acme")
			f.store.statusErr = refused
			rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
			want := http.StatusConflict
			if errors.Is(refused, identity.ErrAdminAuthority) {
				want = http.StatusUnauthorized
			}
			if rec.Code != want {
				t.Fatalf("got %d want %d: %s", rec.Code, want, rec.Body.String())
			}
			if len(f.audit.entries) != 0 {
				t.Fatal("refusal recorded as completed disable")
			}
		})
	}
}
