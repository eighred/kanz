package execution

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
)

func TestVenueDecimalPreservesEveryDigit(t *testing.T) {
	for _, text := range []string{
		"0.000000001", "1.000000001", "1.000000009", "-0.000000009",
		"90071992547409.93", "9223372036854775807", "-9223372036854775808", "9.223372036854775807",
		"0." + strings.Repeat("0", 59) + "1", "1" + strings.Repeat("0", 60),
	} {
		t.Run(text, func(t *testing.T) {
			d, ok := ParseDec(text)
			if !ok {
				t.Fatal("refused an exact supported value")
			}
			want, _ := new(big.Rat).SetString(text)
			if got := dec.FromProto(d); got.Cmp(want) != 0 {
				t.Fatalf("parsed %s as %s", text, got.RatString())
			}
			if got, err := FormatDec(d); err != nil || got != text {
				t.Fatalf("wire value = %q, want %q", got, text)
			}
		})
	}
}

func TestVenueDecimalFormatRefusesUnknownAndUnsupported(t *testing.T) {
	for _, d := range []*commonpb.Decimal{
		nil, {Coefficient: 1, Exponent: 2147483647},
		{Coefficient: 1, Exponent: -2147483648},
		{Coefficient: 1, Exponent: -61}, {Coefficient: 1, Exponent: 61},
	} {
		if text, err := FormatDec(d); !errors.Is(err, ErrVenueDecimal) || text != "" {
			t.Fatalf("FormatDec(%v) = %q, %v; want empty refusal", d, text, err)
		}
		if value, ok := SubDec(d, &commonpb.Decimal{}); ok || value != nil {
			t.Fatalf("SubDec(%v, 0) = %v, %v; want refusal", d, value, ok)
		}
		if value, err := LeavesRemaining(d, &commonpb.Decimal{}); err == nil || value != nil {
			t.Fatalf("LeavesRemaining(%v, 0) = %v, %v; want refusal", d, value, err)
		}
	}
}

func TestVenueDecimalNormalizesScaleWithoutRounding(t *testing.T) {
	for _, d := range []*commonpb.Decimal{
		{Coefficient: 1000000000, Exponent: -9}, {Coefficient: 10, Exponent: -1},
		{Coefficient: 1},
	} {
		if text, err := FormatDec(d); err != nil || text != "1" {
			t.Fatalf("FormatDec(%v) = %q, %v; want 1", d, text, err)
		}
	}
	if text, err := FormatDec(&commonpb.Decimal{}); err != nil || text != "0" {
		t.Fatalf("known zero = %q, %v", text, err)
	}
	// The result needs twenty significant digits. It must not round to int64.
	if value, ok := SubDec(&commonpb.Decimal{Coefficient: 9223372036854775807},
		&commonpb.Decimal{Coefficient: 1, Exponent: -9}); ok || value != nil {
		t.Fatalf("unrepresentable residual = %v, %v", value, ok)
	}
}

func TestVenueDecimalRefusesRatherThanApproximating(t *testing.T) {
	for _, text := range []string{
		"1.1234567890123456789", "9223372036854775808", "1/3",
		"0." + strings.Repeat("0", 60) + "1", "1" + strings.Repeat("0", 61),
		strings.Repeat("9", 401), "1e999999999", "0x1p999999999", "NaN", "",
		"1e-9", "+1", ".5", "1.", "0x1p-9",
	} {
		t.Run(text, func(t *testing.T) {
			if d, ok := ParseDec(text); ok || d != nil {
				t.Fatalf("unrepresentable/invalid input accepted: %v, %v", d, ok)
			}
		})
	}
}

func TestVenueLeavesRetainsSubEightPlaceResidual(t *testing.T) {
	// Construct independently of ParseDec: a rounding parser must not make the
	// expected residual disappear in the fixture as well.
	ordered := &commonpb.Decimal{Coefficient: 1000000009, Exponent: -9}
	cumulative := &commonpb.Decimal{Coefficient: 1}
	leaves, err := LeavesRemaining(ordered, cumulative)
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.FromProto(leaves); got.Cmp(big.NewRat(9, 1000000000)) != 0 {
		t.Fatalf("remaining quantity = %s, want 9/1000000000", got.RatString())
	}
	if _, err := LeavesRemaining(cumulative, ordered); err == nil {
		t.Fatal("sub-eight-place overfill accepted")
	}
}
