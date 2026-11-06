package main

import "testing"

func TestKafkaIdentitySelection(t *testing.T) {
	for _, tt := range []struct {
		value, socket  string
		enabled, valid bool
	}{
		{"", "", false, true}, {"false", "", false, true}, {"false", "unix:///fixture", false, true},
		{"true", "unix:///fixture", true, true}, {"true", "", false, false}, {"TRUE", "unix:///fixture", false, false},
		{"typo", "unix:///fixture", false, false},
	} {
		enabled, err := kafkaIdentityRequested(tt.value, tt.socket)
		if (err == nil) != tt.valid || enabled != tt.enabled {
			t.Fatalf("selection %q: enabled=%v valid=%v", tt.value, enabled, err == nil)
		}
	}
}
