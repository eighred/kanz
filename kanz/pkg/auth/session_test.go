package auth

import "testing"

func TestSessionEpochExactParsing(t *testing.T) {
	for _, v := range []any{nil, float64(1), "", "-1", "01", "+1", "1.0", "9223372036854775808", true} {
		if _, err := SessionEpoch(map[string]any{ClaimSessionEpoch: v}); err == nil {
			t.Errorf("accepted malformed epoch %v", v)
		}
	}
	for _, v := range []string{"0", "1", "9007199254740993", "9223372036854775807"} {
		if _, err := SessionEpoch(map[string]any{ClaimSessionEpoch: v}); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	if n, err := SessionEpoch(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}
