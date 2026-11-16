package tca

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestMeasureRefusesNonzeroFeesWithoutCurrency(t *testing.T) {
	for _, signed := range []struct {
		name   string
		amount int64
	}{{"charge", 1}, {"rebate", -1}} {
		t.Run(signed.name, func(t *testing.T) {
			for _, position := range []string{"alone", "before_known", "after_known"} {
				t.Run(position, func(t *testing.T) {
					unknown := fill(1, 100, 0, "")
					// The existing fill helper omits Money when currency is empty;
					// construct the actual nonzero, unlabelled fee explicitly.
					unknown.Fee = &commonpb.Money{Amount: d(signed.amount, 0)}
					known := fill(1, 100, 1, "USD")
					fills := []*orderpb.Fill{unknown}
					switch position {
					case "before_known":
						fills = append(fills, known)
					case "after_known":
						fills = []*orderpb.Fill{known, unknown}
					}
					got, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)), fills)
					if !errors.Is(err, ErrMissingFeeCurrency) {
						t.Fatalf("unknown fee units = %+v, %v; want ErrMissingFeeCurrency", got, err)
					}
					if !reflect.DeepEqual(got, Result{}) {
						t.Fatalf("refusal leaked a partial measurement: %+v", got)
					}
				})
			}
		})
	}
}

func TestMeasureFeeCurrencyKnownAndZeroControls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fills    []*orderpb.Fill
		fee, bps *big.Rat
		currency string
	}{
		{"nil_fee", []*orderpb.Fill{fill(1, 100, 0, "")}, big.NewRat(0, 1), big.NewRat(0, 1), ""},
		{"zero_without_currency", []*orderpb.Fill{{Quantity: d(1, 0), Price: d(100, 0), Fee: &commonpb.Money{Amount: d(0, 0)}}}, big.NewRat(0, 1), big.NewRat(0, 1), ""},
		{"zero_with_currency", []*orderpb.Fill{fill(1, 100, 0, "USD")}, big.NewRat(0, 1), big.NewRat(0, 1), "USD"},
		{"known_charge", []*orderpb.Fill{fill(1, 100, 1, "USD")}, big.NewRat(1, 1), big.NewRat(100, 1), "USD"},
		{"known_rebate", []*orderpb.Fill{fill(1, 100, -1, "USD")}, big.NewRat(-1, 1), big.NewRat(-100, 1), "USD"},
		{"existing_rebate_convention", []*orderpb.Fill{fill(10, 100, -5, "USD")}, big.NewRat(-5, 1), big.NewRat(-50, 1), "USD"},
		{"same_currency_accumulation", []*orderpb.Fill{fill(1, 100, 2, "USD"), fill(1, 100, -1, "USD")}, big.NewRat(1, 1), big.NewRat(50, 1), "USD"},
		{"absent_fee_before_known", []*orderpb.Fill{fill(1, 100, 0, ""), fill(1, 100, 1, "USD")}, big.NewRat(1, 1), big.NewRat(50, 1), "USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Measure(order(orderpb.Side_SIDE_BUY, d(100, 0)), tc.fills)
			// At the arrival price there is no price shortfall. One USD fee on
			// 100 USD notional is 100 bps; 1 USD on 200 USD is 50 bps.
			if err != nil {
				t.Fatal(err)
			}
			if got.Fees.Cmp(tc.fee) != 0 || got.ShortfallBps.Cmp(tc.bps) != 0 || got.PriceShortfallBps.Sign() != 0 || got.FeeCurrency != tc.currency {
				t.Fatalf("result = %+v, want fee=%s %s and %s bps", got, tc.fee, tc.currency, tc.bps)
			}
		})
	}
}
