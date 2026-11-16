package governed

import (
	"context"
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

func TestScenarioFractionExactOrRefused(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"ordinary", "-0.05", "-1/20"},
		{"tiny", "0.0000001", "1/10000000"},
		{"precise", "0.123456789012345678", "61728394506172839/500000000000000000"},
		{"integer_beyond_float", "9007199254740993", "9007199254740993"},
		{"max_coefficient", "9223372036854775807", "9223372036854775807"},
		{"min_coefficient", "-9223372036854775808", "-9223372036854775808"},
		{"lower_exponent", "1e-60", "1/" + "1" + strings.Repeat("0", 60)},
		{"upper_exponent", "1e60", "1" + strings.Repeat("0", 60)},
		{"plus", "+0.25", "1/4"},
		{"leading_dot", "-.125", "-1/8"},
		{"trailing_dot", "2.", "2"},
		{"scientific", "+1.25E-7", "1/8000000"},
		{"zero", "0", "0"},
		{"negative_zero", "-0.000e+2", "0"},
		{"over_coefficient", "9223372036854775808", ""},
		{"under_coefficient", "-9223372036854775809", ""},
		{"too_precise", "0.12345678901234567891", ""},
		{"below_domain", "1e-61", ""},
		{"above_domain", "1e61", ""},
		{"nan", "NaN", ""},
		{"inf", "+Inf", ""},
		{"negative_inf", "-Infinity", ""},
		{"overflow", "1e309", ""},
		{"huge_exponent", "1e999999999999999999999", ""},
		{"tiny_exponent", "1e-999999999", ""},
		{"oversize", strings.Repeat("0", 401), ""},
		{"rational", "1/2", ""},
		{"hex", "0x1p-1", ""},
		{"underscore", "1_000", ""},
		{"space", " 0.5", ""},
		{"empty", "", ""},
		{"named", "GFC", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var sent *commonpb.Decimal
			c := NewGRPCClient(fakeQuery{scenario: func(r *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
				calls++
				if r.GetPortfolioId() != "PF" || len(r.GetShocks()) != 1 {
					t.Fatalf("request identity/shape changed: %v", r)
				}
				sent = r.GetShocks()[0].GetParallelShift().GetPct()
				return &querypb.EvaluateScenarioResponse{}, nil
			}})
			_, err := c.EvaluateScenario(context.Background(), "PF", tc.input)
			if tc.want == "" {
				if err == nil || calls != 0 {
					t.Fatalf("unsupported fraction reached risk: calls=%d err=%v sent=%v", calls, err, sent)
				}
				return
			}
			if err != nil || calls != 1 || sent == nil {
				t.Fatalf("supported fraction refused: calls=%d err=%v sent=%v", calls, err, sent)
			}
			// Decode the wire representation independently of the production converter.
			power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absExponent(sent.GetExponent()))), nil)
			actual := new(big.Rat).SetInt64(sent.GetCoefficient())
			if sent.GetExponent() < 0 {
				actual.Quo(actual, new(big.Rat).SetInt(power))
			} else {
				actual.Mul(actual, new(big.Rat).SetInt(power))
			}
			want, ok := new(big.Rat).SetString(tc.want)
			if !ok || actual.Cmp(want) != 0 {
				t.Fatalf("fraction changed: got %s (%v), want %s", actual, sent, tc.want)
			}
		})
	}
}

func absExponent(n int32) int32 {
	if n < 0 {
		return -n
	}
	return n
}
