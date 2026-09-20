package config

import (
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestIdentityAdministrationRoleIsExplicitAndSeparate(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL_FILE", "")
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://unused")
	t.Setenv("IDENTITY_TOKEN_ISSUER", "https://identity.test")
	t.Setenv("IDENTITY_TOKEN_AUDIENCE", "kanz")
	t.Setenv("IDENTITY_SIGNING_KEY_FILE", "")
	t.Setenv("IDENTITY_ALLOW_EPHEMERAL_KEY", "true")
	t.Setenv("IDENTITY_INVITE_EMAIL_DOMAINS", "")
	for _, tc := range []struct {
		role, legacy string
		valid        bool
	}{
		{"", "", true}, {identity.AdminRole, "", true},
		{"kanz-operator", "", false}, {"kanz-trader", "", false},
		{"", "kanz-operator", false}, {identity.AdminRole, "kanz-operator", false},
	} {
		t.Run(tc.role+"/"+tc.legacy, func(t *testing.T) {
			t.Setenv("IDENTITY_ADMIN_ROLE", tc.role)
			t.Setenv("IDENTITY_OPERATOR_ROLE", tc.legacy)
			cfg, err := Load()
			if (err == nil) != tc.valid {
				t.Fatalf("role=%q legacy=%q: %v", tc.role, tc.legacy, err)
			}
			if err == nil && cfg.AdminRole != tc.role {
				t.Fatal("configuration lost role")
			}
		})
	}
}
