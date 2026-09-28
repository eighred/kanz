package auth

import (
	"testing"
	"time"
)

func TestMFAAssuranceParsingAndFreshness(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	good := MFA{Required: true, VerifiedAt: now.Add(-time.Minute)}
	parsed, err := ParseMFA(map[string]any{ClaimMFA: good.Claim()}, now)
	if err != nil || parsed != good || !parsed.Recent(now) {
		t.Fatal("valid assurance rejected", err)
	}
	if m, e := ParseMFA(nil, now); e != nil || m.Required || m.Recent(now) {
		t.Fatal("absence fabricated assurance")
	}
	for _, value := range []any{nil, true, "webauthn", map[string]any{"method": "pwd", "verified_at": "1800000000"}, map[string]any{"method": "webauthn-uv", "verified_at": 1800000000}, map[string]any{"method": "webauthn-uv", "verified_at": "1800000001"}, map[string]any{"method": "webauthn-uv", "verified_at": "01800000000"}, map[string]any{"method": "webauthn-uv", "verified_at": "0"}, map[string]any{"method": "webauthn-uv", "verified_at": "1800000000", "extra": true}} {
		if _, e := ParseMFA(map[string]any{ClaimMFA: value}, now); e == nil {
			t.Fatal("malformed assurance accepted")
		}
	}
	for _, at := range []time.Time{{}, now.Add(time.Second), now.Add(-StepUpTTL)} {
		if (MFA{Required: true, VerifiedAt: at}).Recent(now) {
			t.Fatal("unproven or expired assurance accepted")
		}
	}
}
