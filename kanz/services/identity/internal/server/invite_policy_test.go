package server

import (
	"net/http"
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestProvisionInviteDomainBoundary(t *testing.T) {
	for _, tc := range []struct {
		subject string
		status  int
	}{
		{"user:a@eighred.co", http.StatusCreated},
		{"user:a@gmail.com", http.StatusForbidden},
		{"user:a@eighred.co.attacker.com", http.StatusForbidden},
		{"user:a@sub.eighred.co", http.StatusForbidden},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			f := newProvServer(t, operatorClaims(), nil)
			p, err := identity.ParseInviteDomainPolicy("eighred.co")
			if err != nil {
				t.Fatal(err)
			}
			f.s.provisioning.InviteDomains = p
			body := validBody()
			body["subject"] = tc.subject
			r := f.req(t, http.MethodPost, "/invites", "tok", body, nil)
			if r.Code != tc.status {
				t.Fatalf("status %d: %s", r.Code, r.Body.String())
			}
			if tc.status == http.StatusForbidden && len(f.store.created) != 0 {
				t.Fatal("denied invitation persisted")
			}
		})
	}
}
