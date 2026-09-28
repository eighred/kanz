package config

import "testing"

func TestRecoveryConfigurationFailsClosed(t *testing.T) {
	t.Setenv("IDENTITY_RECOVERY_ENABLED", "true")
	t.Setenv("IDENTITY_SMTP_ADDRESS", "mail.eighred.com:587")
	t.Setenv("IDENTITY_SMTP_FROM", "noreply@eighred.com")
	t.Setenv("IDENTITY_SMTP_TLS", "starttls")
	t.Setenv("IDENTITY_RECOVERY_ORIGIN", "https://kanz.eighred.com")
	t.Setenv("IDENTITY_SMTP_PASSWORD_FILE", "")
	t.Setenv("IDENTITY_SMTP_PASSWORD", "")
	t.Setenv("IDENTITY_SMTP_USERNAME", "")
	if _, err := loadRecovery(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"IDENTITY_RECOVERY_ENABLED", "yes"}, {"IDENTITY_SMTP_ADDRESS", "mail.eighred.com"}, {"IDENTITY_SMTP_FROM", "Name <noreply@eighred.com>"}, {"IDENTITY_SMTP_TLS", "none"}, {"IDENTITY_RECOVERY_ORIGIN", "http://kanz.eighred.com"}, {"IDENTITY_RECOVERY_ORIGIN", "https://kanz.eighred.com/path"}, {"IDENTITY_RECOVERY_ORIGIN", "https://attacker@kanz.eighred.com"}, {"IDENTITY_SMTP_USERNAME", "without-password"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := loadRecovery(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
