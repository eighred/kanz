package config

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

func TestSessionConfigurationRefusesPartialOrInconsistentSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		valid  bool
	}{
		{"default", nil, true},
		{"unknown mode", map[string]string{"WEB_BFF_SESSION_MODE": "redis"}, false},
		{"missing key", map[string]string{"WEB_BFF_SESSION_MODE": "postgres", "WEB_BFF_SESSION_DSN": "synthetic"}, false},
		{"memory with shared settings", map[string]string{"WEB_BFF_SESSION_DSN": "synthetic"}, false},
		{"invalid ttl", map[string]string{"WEB_BFF_SESSION_TTL": "bad"}, false},
		{"oversized ttl", map[string]string{"WEB_BFF_SESSION_TTL": "25h"}, false},
		{"zero capacity", map[string]string{"WEB_BFF_MAX_SESSIONS": "0"}, false},
		{"inconsistent quota", map[string]string{"WEB_BFF_MAX_SESSIONS": "1", "WEB_BFF_MAX_SESSIONS_PER_SUBJECT": "2"}, false},
		{"shared", map[string]string{"WEB_BFF_SESSION_MODE": "postgres", "WEB_BFF_SESSION_DSN": "synthetic", "WEB_BFF_SESSION_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"WEB_BFF_SESSION_MODE", "WEB_BFF_SESSION_DSN", "WEB_BFF_SESSION_DSN_FILE", "WEB_BFF_SESSION_KEY", "WEB_BFF_SESSION_KEY_FILE", "WEB_BFF_SESSION_TTL"} {
				t.Setenv(key, "")
			}
			for k, v := range tc.values {
				t.Setenv(k, v)
			}
			c := Config{SessionTTL: time.Hour}
			e := loadSessions(&c)
			if (e == nil) != tc.valid {
				t.Fatal("configuration outcome", e)
			}
		})
	}
}
