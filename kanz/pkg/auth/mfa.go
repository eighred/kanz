package auth

import (
	"errors"
	"strconv"
	"time"
)

const ClaimMFA = "kanz_mfa"
const StepUpTTL = 5 * time.Minute

// MFA is signed assurance, never a browser header or an inferred role. Expiry
// of step-up removes privileged authority without destroying read-only access.
type MFA struct {
	Required   bool
	VerifiedAt time.Time
}

func (m MFA) Recent(now time.Time) bool {
	return m.Required && !m.VerifiedAt.IsZero() && !m.VerifiedAt.After(now) && now.Sub(m.VerifiedAt) < StepUpTTL
}

func (m MFA) Claim() map[string]any {
	return map[string]any{"method": "webauthn-uv", "verified_at": strconv.FormatInt(m.VerifiedAt.Unix(), 10)}
}

// ParseMFA refuses malformed or future assurance rather than downgrading it to
// an ordinary session. Absence alone denotes a session without native MFA.
func ParseMFA(claims map[string]any, now time.Time) (MFA, error) {
	v, exists := claims[ClaimMFA]
	if !exists {
		return MFA{}, nil
	}
	bad := errors.New("invalid MFA assurance")
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m["method"] != "webauthn-uv" {
		return MFA{}, bad
	}
	s, ok := m["verified_at"].(string)
	if !ok {
		return MFA{}, bad
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s || n > now.Unix() {
		return MFA{}, bad
	}
	return MFA{Required: true, VerifiedAt: time.Unix(n, 0).UTC()}, nil
}
