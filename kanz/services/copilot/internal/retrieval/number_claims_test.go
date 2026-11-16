package retrieval

import (
	"math"
	"reflect"
	"testing"
)

func TestGroundingWholeNumericClaims(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		cited        []float64
		unsupported  []string
	}{
		{"negative_mismatch", "Delta is -0.42.", []float64{0.42}, []string{"-0.42"}},
		{"positive_mismatch", "Delta is +0.42.", []float64{-0.42}, []string{"+0.42"}},
		{"unicode_minus", "Delta is −0.42.", []float64{0.42}, []string{"−0.42"}},
		{"negative_backed", "Delta is -0.42.", []float64{-0.42}, nil},
		{"positive_backed", "Delta is +0.42.", []float64{0.42}, nil},
		{"unicode_minus_backed", "Delta is −0.42.", []float64{-0.42}, nil},
		{"exponent_magnitude", "Risk is 1e+9.", []float64{0.42}, []string{"1e+9"}},
		{"trailing_decimal_exponent_magnitude", "Risk is 1.e+9.", []float64{0.42}, []string{"1.e+9"}},
		{"negative_exponent_magnitude", "Risk is -1E+9.", []float64{1e9}, []string{"-1E+9"}},
		{"scientific_backed", "Risk is 1e+9.", []float64{1e9}, nil},
		{"trailing_decimal_exponent_backed", "Risk is 1.e+9.", []float64{1e9}, nil},
		{"small_exponent_backed", "Delta is -4.2e-1.", []float64{-0.42}, nil},
		{"leading_decimal", "Delta is +.42.", []float64{0.42}, nil},
		{"grouped_backed", "VaR99 is $1,250,000.00.", []float64{1250000}, nil},
		{"grouped_exponent", "Risk is 1,250e3.", []float64{1250000}, nil},
		{"parenthesized", "Delta (-0.42), then (+0.42).", []float64{0.42}, []string{"-0.42"}},
		{"punctuation", "Delta:0.42,next:0.42; risk...0.42!", []float64{0.42}, nil},
		{"range", "Exposure 42.5-43.5.", []float64{42.5, 43.5}, nil},
		{"identifiers", "VaR99 model_123 PF-1234 42bps 99th α1234 information nanotechnology.", nil, nil},
		{"chained_identifiers", "PF-1234-5678 model-v2-999 and risk-book-9876.", nil, nil},
		{"identifier_then_claim", "PF-1234 has Delta -0.42.", []float64{0.42}, []string{"-0.42"}},
		{"sorted_identity", "Risk 1e9, Delta −0.42 and -0.42.", nil, []string{"-0.42", "1e9", "−0.42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckGrounding(tc.answer, tc.cited)
			if got.Grounded != (len(tc.unsupported) == 0) || !reflect.DeepEqual(got.Ungrounded, tc.unsupported) {
				t.Fatalf("grounding %q = %+v, want unsupported %v", tc.answer, got, tc.unsupported)
			}
		})
	}
}

func TestGroundingRefusesUnsupportedNumericClaims(t *testing.T) {
	for _, token := range []string{
		"1e+", "1e-", "1e--9", "1e+-9", "1eX", "1e9.5", "1.2.3", "1_000",
		"0x1p4", "0b1101", "0o777", "NaN", "+Inf", "-Infinity", "∞", "−∞",
		"1e309", "1e-9999", "1e-324", "--0.42", "1,,250", "12,34",
	} {
		t.Run(token, func(t *testing.T) {
			got := CheckGrounding("Risk is "+token+".", []float64{0.42, 1000, math.Inf(1), math.NaN()})
			if got.Grounded || !reflect.DeepEqual(got.Ungrounded, []string{token}) {
				t.Fatalf("unsupported whole claim %q = %+v", token, got)
			}
		})
	}
}

func TestGroundingFiniteMatchingPreservesPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		cited       []float64
		grounded    bool
	}{
		{"round_absolute", "0.429", []float64{0.42}, true},
		{"outside_absolute", "0.431", []float64{0.42}, false},
		{"round_relative", "100.9", []float64{100}, true},
		{"outside_relative", "102", []float64{100}, false},
		{"positive_percentage", "42", []float64{0.42}, true},
		{"negative_percentage", "-42", []float64{-0.42}, true},
		{"percentage_reverse", "0.42", []float64{42}, true},
		{"negative_percentage_reverse", "-0.42", []float64{-42}, true},
		{"near_zero_policy", "-0.001", []float64{0.001}, true},
		{"count_boundary", "12", nil, true},
		{"count_outside", "13", nil, false},
		{"year_lower", "1900", nil, true},
		{"year_upper", "2100", nil, true},
		{"year_below", "1899", nil, false},
		{"year_above", "2101", nil, false},
		{"scientific_count", "1e1", nil, true},
		{"scientific_year", "2e3", nil, true},
		{"negative_count", "-3", nil, false},
		{"positive_zero", "+0", nil, true},
		{"negative_zero", "-0.0", nil, true},
		{"scientific_zero", "−0e9999", nil, true},
		{"percentage_overflow", "1e308", []float64{0.42}, false},
		{"negative_percentage_overflow", "-1e308", []float64{0.42}, false},
		{"finite_large_percentage", "1e308", []float64{1e306}, true},
		{"finite_large_reverse", "1e306", []float64{1e308}, true},
		{"infinite_citation", "123.45", []float64{math.Inf(1), math.Inf(-1)}, false},
		{"nan_citation", "123.45", []float64{math.NaN()}, false},
		{"finite_citation_survives", "0.42", []float64{math.NaN(), math.Inf(1), 0.42}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckGrounding("Risk is "+tc.token+".", tc.cited)
			if got.Grounded != tc.grounded {
				t.Fatalf("claim %q cited %v = %+v, want grounded %v", tc.token, tc.cited, got, tc.grounded)
			}
		})
	}
}
