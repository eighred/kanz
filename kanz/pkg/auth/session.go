package auth

import (
	"fmt"
	"strconv"
)

// ClaimSessionEpoch carries a decimal string, avoiding JSON number precision loss.
const ClaimSessionEpoch = "session_epoch"

// SessionEpoch reads the signed claim. Absence denotes a legacy generation-zero
// token; malformed or negative generations are never treated as absence.
func SessionEpoch(claims map[string]any) (int64, error) {
	v, ok := claims[ClaimSessionEpoch]
	if !ok {
		return 0, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("auth: invalid session epoch")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("auth: invalid session epoch")
	}
	return n, nil
}
