package credit_test

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/credit"
	"github.com/eighred/kanz/internal/risk/xva"
)

type unusableDiscount float64

func (d unusableDiscount) Discount(float64) float64 { return float64(d) }

func TestUnusableCDSRefreshRetainsTheActualStoredCurve(t *testing.T) {
	for _, tc := range []struct {
		name     string
		quotes   []xva.CDSQuote
		recovery float64
		disc     credit.DiscountSource
	}{
		{"NaN spread", []xva.CDSQuote{{Tenor: 1, Spread: math.NaN()}}, 0.4, nil},
		{"NaN tenor", []xva.CDSQuote{{Tenor: math.NaN(), Spread: 0.01}}, 0.4, nil},
		{"NaN recovery", termStructure(), math.NaN(), nil},
		{"infinite spread", []xva.CDSQuote{{Tenor: 1, Spread: math.Inf(1)}}, 0.4, nil},
		{"NaN discount", termStructure(), 0.4, unusableDiscount(math.NaN())},
		{"zero discount", termStructure(), 0.4, unusableDiscount(0)},
		{"negative discount", termStructure(), 0.4, unusableDiscount(-1)},
		{"overflowed annuity", termStructure(), 0.4, unusableDiscount(math.MaxFloat64)},
		{"underflowed annuity", termStructure(), 0.4, unusableDiscount(math.SmallestNonzeroFloat64)},
		{"underflowed positive leg", termStructure(), 0.4, unusableDiscount(1e-322)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{quotes: termStructure(), recovery: 0.4}
			cal, st := newCal(src)
			good, err := cal.Refresh(ctx, "ACME", base)
			if err != nil {
				t.Fatal(err)
			}
			before := xva.CreditCurve{Recovery: good.Recovery, Tenors: append([]float64(nil), good.Tenors...), Hazards: append([]float64(nil), good.Hazards...)}
			src.quotes, src.recovery, cal.Disc = tc.quotes, tc.recovery, tc.disc
			later := base.Add(time.Hour)
			if c, err := cal.Refresh(ctx, "ACME", later); !errors.Is(err, xva.ErrCDSBootstrap) || c != nil {
				t.Fatalf("invalid refresh succeeded or lost bootstrap sentinel: curve=%+v error=%v", c, err)
			}
			for _, at := range []time.Time{base, later, later.Add(time.Hour)} {
				stored, ok := st.Curve(ctx, "ACME", at)
				if !ok || stored != good || !reflect.DeepEqual(*stored, before) {
					t.Fatalf("failed refresh replaced or mutated the actual stored curve at %v: %+v", at, stored)
				}
			}
			if _, ok := st.Curve(ctx, "OTHER", later); ok {
				t.Fatal("failed ACME refresh fabricated another reference curve")
			}
			// Failure is not a permanent latch. A later valid quote set still
			// publishes normally without rewriting the earlier PIT version.
			src.quotes, src.recovery, cal.Disc = termStructure(), 0.4, credit.FlatRate(0.03)
			recovered, err := cal.Refresh(ctx, "ACME", later.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if stored, ok := st.Curve(ctx, "ACME", later.Add(2*time.Hour)); !ok || stored != recovered {
				t.Fatal("valid refresh did not recover publication")
			}
			if stored, _ := st.Curve(ctx, "ACME", base); stored != good {
				t.Fatal("recovery rewrote the historical curve")
			}
		})
	}
}
