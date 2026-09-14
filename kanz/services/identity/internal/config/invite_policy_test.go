package config

import "testing"

func TestLoadInviteDomainPolicy(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL_FILE", "")
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://unused")
	t.Setenv("IDENTITY_TOKEN_ISSUER", "https://identity.test")
	t.Setenv("IDENTITY_TOKEN_AUDIENCE", "kanz")
	t.Setenv("IDENTITY_SIGNING_KEY_FILE", "")
	t.Setenv("IDENTITY_ALLOW_EPHEMERAL_KEY", "true")
	t.Setenv("IDENTITY_INVITE_EMAIL_DOMAINS", "eighred.co")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.InviteDomains.Check("user:a@gmail.com"); err == nil {
		t.Fatal("policy not loaded")
	}
	t.Setenv("IDENTITY_INVITE_EMAIL_DOMAINS", "eighred.co,")
	if _, err := Load(); err == nil {
		t.Fatal("invalid policy did not fail startup")
	}
}
