package dec

import (
	"strings"
	"testing"
)

func TestProtoExactPreservesOrRefuses(t *testing.T) {
	for _, text := range []string{"0", "-0.000000000001", "1000000000000", "9007199254740993", "1/8", "9223372036854775807", "-9223372036854775808"} {
		d, ok := ParseProtoExact(text)
		if !ok || FromProto(d).Cmp(Rat(text)) != 0 {
			t.Fatalf("changed exact input %s: %v", text, d)
		}
	}
	for _, text := range []string{"", "1/3", "9223372036854775808", "0.12345678901234567891", "1e999999999", "NaN", strings.Repeat("9", 401), "1/0"} {
		if d, ok := ParseProtoExact(text); ok || d != nil {
			t.Fatalf("accepted unrepresentable input %q", text)
		}
	}
	if d, ok := ToProtoExact(nil); ok || d != nil {
		t.Fatal("missing converted to zero")
	}
}
