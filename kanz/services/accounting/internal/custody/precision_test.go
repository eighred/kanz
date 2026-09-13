package custody

import (
	"math/big"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
)

func TestCustodyExactRenderingAndWireRefusal(t *testing.T) {
	for _, text := range []string{"0", "-0.000000000001", "1.000000015", "100000000000000000000"} {
		r := dec.Rat(text)
		rendered, ok := ExactDecimalText(r)
		if !ok || rendered != text {
			t.Fatalf("text %s: %s %v", text, rendered, ok)
		}
		wire, ok := exactProto(r)
		if !ok || dec.FromProto(wire).Cmp(r) != 0 {
			t.Fatalf("wire rounded %s: %v", text, wire)
		}
	}
	for _, r := range []*big.Rat{nil, big.NewRat(1, 3), new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 513))} {
		if _, ok := ExactDecimalText(r); ok {
			t.Fatal("unavailable value rendered as exact")
		}
		if _, ok := exactProto(r); ok {
			t.Fatal("unavailable value published")
		}
	}
	// Previously accepted with rounded low digits. The wire coefficient cannot
	// hold this exact value; publishing must refuse rather than announce an approximation.
	if _, ok := exactProto(dec.Rat("9223372036854.775808e3")); ok {
		t.Fatal("unrepresentable wire coefficient accepted")
	}
	for _, text := range []string{"", "NaN", "1e999999999", "1/0", strings.Repeat("9", 401)} {
		if _, err := parseStored(text); err == nil {
			t.Fatalf("invalid durable input accepted: %q", text)
		}
	}
	r := big.NewRat(1, 3)
	stored, err := exactStored(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseStored(stored)
	if err != nil || got.Cmp(r) != 0 {
		t.Fatalf("rational storage: %v %v", got, err)
	}
}
