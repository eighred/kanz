package execution

import (
	"errors"
	"fmt"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestValidateExecutedPrice(t *testing.T) {
	positive := &commonpb.Decimal{Coefficient: 1}
	for _, tc := range []struct {
		name            string
		quantity, price *commonpb.Decimal
		refuse          bool
	}{
		{"zero", positive, &commonpb.Decimal{}, true},
		{"negative", positive, &commonpb.Decimal{Coefficient: -1}, true},
		{"missing", positive, nil, true},
		{"unsupported", positive, &commonpb.Decimal{Coefficient: 1, Exponent: 65}, true},
		{"missing_quantity", nil, positive, true},
		{"unsupported_quantity", &commonpb.Decimal{Coefficient: 1, Exponent: 65}, positive, true},
		{"tiny_positive", positive, &commonpb.Decimal{Coefficient: 1, Exponent: -60}, false},
		{"large_positive", positive, &commonpb.Decimal{Coefficient: 1, Exponent: 60}, false},
		{"zero_size_no_execution_price", &commonpb.Decimal{}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExecutedPrice(tc.quantity, tc.price)
			if tc.refuse {
				if !errors.Is(err, ErrVenueExecutionPrice) || RefusalKind(fmt.Errorf("wrapped: %w", err)) != "invalid_price" {
					t.Fatalf("refusal lost its bounded identity: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	// The price policy must not change general signed parsing used by fees.
	if d, ok := ParseDec("-0.000000003"); !ok || d.GetCoefficient() >= 0 {
		t.Fatalf("signed fee Decimal was refused: %v, %v", d, ok)
	}
}
