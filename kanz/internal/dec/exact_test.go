package dec

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func TestExactNeverRoundsOrAcceptsJSONNumbers(t *testing.T) {
	for _, s := range []string{"9007199254740993.000000001", "1/3", "-0.000000000000000000001", "0"} {
		n, err := ParseExact(s)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var out Exact
		if err := json.Unmarshal(raw, &out); err != nil || out != n {
			t.Fatal(out, err)
		}
		r, _ := out.Rat()
		want, _ := new(big.Rat).SetString(s)
		if r.Cmp(want) != 0 {
			t.Fatal(out, s)
		}
	}
	var out Exact
	if json.Unmarshal([]byte(`9007199254740993`), &out) == nil {
		t.Fatal("legacy numeric JSON admitted")
	}
	for _, s := range []string{"", "NaN", "Infinity", "1e999999999", "1/0", "1/-3", strings.Repeat("9", 401)} {
		if _, err := ParseExact(s); err == nil {
			t.Fatal("invalid exact input", s)
		}
	}
}
