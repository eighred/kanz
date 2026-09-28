package config

import "testing"

func TestWebAuthnConfigurationFailsClosed(t *testing.T) {
	t.Setenv("IDENTITY_MFA_ENABLED", "true")
	t.Setenv("IDENTITY_MFA_REQUIRED", "true")
	t.Setenv("IDENTITY_WEBAUTHN_ORIGIN", "https://kanz.example.test")
	t.Setenv("IDENTITY_WEBAUTHN_RP_ID", "kanz.example.test")
	if c, e := loadMFA(); e != nil || c == nil || !c.RequirePrivileged {
		t.Fatal("valid configuration rejected", e)
	}
	for _, tc := range []struct{ k, v string }{{"IDENTITY_MFA_ENABLED", "false"}, {"IDENTITY_MFA_ENABLED", "yes"}, {"IDENTITY_MFA_REQUIRED", "yes"}, {"IDENTITY_WEBAUTHN_ORIGIN", "http://kanz.example.test"}, {"IDENTITY_WEBAUTHN_ORIGIN", "https://kanz.example.test/path"}, {"IDENTITY_WEBAUTHN_ORIGIN", "https://attacker@kanz.example.test"}, {"IDENTITY_WEBAUTHN_RP_ID", "example.test"}, {"IDENTITY_WEBAUTHN_RP_ID", ""}} {
		t.Run(tc.k+tc.v, func(t *testing.T) {
			t.Setenv(tc.k, tc.v)
			if _, e := loadMFA(); e == nil {
				t.Fatal("bad configuration accepted")
			}
		})
	}
}
