package performance

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"testing"
)

// Compare the existing statistical reference on the supported intervals only.
// Coverage is decided by the reporting owner before either method runs.
func TestDailyNoFlowReference(t *testing.T) {
	data, err := os.ReadFile("testdata/daily_no_flow_return.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name      string
		Opening   *string
		Closing   *string
		FlowCount *uint64 `json:"flow_count"`
		Fraction  *string
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, v := range vectors {
		if v.Fraction == nil {
			continue
		}
		matched++
		t.Run(v.Name, func(t *testing.T) {
			number := func(raw *string) float64 {
				t.Helper()
				if raw == nil {
					t.Fatal("measured vector needs a value")
				}
				r, ok := new(big.Rat).SetString(*raw)
				if !ok {
					t.Fatal("invalid exact amount")
				}
				x, _ := r.Float64()
				return x
			}
			if v.FlowCount == nil || *v.FlowCount != 0 || number(v.Opening) <= 0 {
				t.Fatal("measured vector lacks complete zero-flow coverage or a positive base")
			}
			got := TimeWeightedReturn([]SubPeriod{{BeginValue: number(v.Opening), EndValue: number(v.Closing)}})
			if want := number(v.Fraction); math.IsNaN(got) || math.Abs(got-want) > 1e-12 {
				t.Fatalf("reference = %.17g, want %.17g", got, want)
			}
		})
	}
	if matched != 5 {
		t.Fatalf("compared %d measured cases", matched)
	}
}

func TestDailyIntradayFlowReference(t *testing.T) {
	for _, tc := range []struct {
		close, linked, closeOnly float64
	}{
		{165, 21.0 / 100, 65.0 / 100},
		{132, -4.0 / 125, 32.0 / 100},
	} {
		before := SubPeriod{BeginValue: 100, EndValue: 110}
		after := SubPeriod{BeginValue: 110, Flow: 40, EndValue: tc.close}
		twr := TimeWeightedReturn([]SubPeriod{before, after})
		change := (tc.close - before.BeginValue) / before.BeginValue
		if !(math.Abs(twr-tc.linked) <= 1e-12 && math.Abs(change-tc.closeOnly) <= 1e-12) {
			t.Fatalf("closing NAV %.0f: linked return %g, close change %g", tc.close, twr, change)
		}
	}
}
