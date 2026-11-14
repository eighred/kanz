package server

import (
	"math/big"
	"strings"
	"testing"
)

func TestAccountingBoundedNumericFormats(t *testing.T) {
	max512 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 512), big.NewInt(1))
	cancel513 := new(big.Int).Lsh(big.NewInt(1), 512).String()
	for _, tc := range []struct {
		name, text string
		want       *big.Rat
	}{
		{"decimal", "1.25", big.NewRat(5, 4)},
		{"repeating_fraction", "1/3", big.NewRat(1, 3)},
		{"signed_fraction", "-3/2", big.NewRat(-3, 2)},
		{"leading_plus", "+.5", big.NewRat(1, 2)},
		{"leading_dot", ".5", big.NewRat(1, 2)},
		{"trailing_dot", "1.", big.NewRat(1, 1)},
		{"decimal_exponent", "1e-3", big.NewRat(1, 1000)},
		{"uppercase_exponent", "1E3", big.NewRat(1000, 1)},
		{"binary_exponent", "1p3", big.NewRat(8, 1)},
		{"hex_e_digit", "0x1e", big.NewRat(30, 1)},
		{"hex_E_digit", "0X1E", big.NewRat(30, 1)},
		{"hex_fraction", "0x1.8p1", big.NewRat(3, 1)},
		{"hex_negative_exponent", "0X1.EP-1", big.NewRat(15, 16)},
		{"binary_prefix", "0b1.1p1", big.NewRat(3, 1)},
		{"octal_prefix", "0o1.4p1", big.NewRat(3, 1)},
		{"prefixed_fraction", "0x10/0b10", big.NewRat(8, 1)},
		{"fraction_octal_numerator", "010/2", big.NewRat(4, 1)},
		{"underscores", "1_000.0_0", big.NewRat(1000, 1)},
		{"exponent_underscores", "1e0_2", big.NewRat(100, 1)},
		{"negative_scientific", "-1e-3", big.NewRat(-1, 1000)},
		{"zero", "0", new(big.Rat)},
		{"negative_zero", "-0", new(big.Rat)},
		{"zero_scientific", "0e155", new(big.Rat)},
		{"hex_zero", "0x0p155", new(big.Rat)},
		{"scientific_512_bit_result", "1e154", new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(154), nil))},
		{"numerator_512_bits", max512.String(), new(big.Rat).SetInt(max512)},
		{"denominator_512_bits", "1/" + max512.String(), new(big.Rat).SetFrac(big.NewInt(1), max512)},
		{"reduced_513_bit_operands", cancel513 + "/" + cancel513, big.NewRat(1, 1)},
		{"text_at_400_bytes", strings.Repeat("0", 399) + "1", big.NewRat(1, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRat(tc.text)
			if err != nil || got == nil || got.Cmp(tc.want) != 0 {
				t.Fatalf("%q: got=%v err=%v want=%s", tc.text, got, err, tc.want.RatString())
			}
		})
	}
}

func TestAccountingNumericRefusals(t *testing.T) {
	bit513 := new(big.Int).Lsh(big.NewInt(1), 512).String()
	for _, tc := range []struct{ name, text string }{
		{"empty", ""}, {"whitespace", " "}, {"NaN", "NaN"}, {"Infinity", "Infinity"},
		{"zero_denominator", "1/0"}, {"negative_denominator", "1/-3"},
		{"bad_underscore", "1__2"}, {"missing_exponent", "1e+"},
		{"hex_junk", "0x1g"}, {"trailing_junk", "1 xyz"},
		{"scientific_result_oversize", "1e155"},
		{"numerator_513_bits", bit513}, {"denominator_513_bits", "1/" + bit513},
		{"text_over_budget_even_when_reducing_small", strings.Repeat("0", 400) + "1"},
		{"fraction_text_over_budget", strings.Repeat("0", 200) + "1/" + strings.Repeat("0", 200) + "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRat(tc.text)
			if err == nil || got != nil {
				t.Fatalf("unqualified input accepted: got=%v err=%v", got, err)
			}
		})
	}
}
