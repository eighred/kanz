package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/pricing"
)

func TestRevaluer_RepricesNonlinearly(t *testing.T) {
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := asOf.AddDate(1, 0, 0)
	const spot, sigma, mult, qty = 100.0, 0.20, 100.0, 10.0

	rv := NewRevaluer(GreeksProviders{
		Terms: staticTerms{"OPT": {UnderlyingID: "UND", Strike: 100, Expiry: expiry, Type: pricing.Call, Exercise: pricing.European, Multiplier: mult}},
		Spot:  staticSpot{"UND": spot},
		Vol:   constVol(sigma),
	})
	ttm := expiry.Sub(asOf).Hours() / 24 / 365
	basePrice := pricing.BlackScholesPrice(pricing.Call, spot, 100, ttm, 0, 0, sigma)
	n := qty * mult
	baseMV := &commonpb.Money{Amount: floatToDecimal(basePrice*n, -2), CurrencyCode: "USD"}

	// Price −10%: the call reprices to its BS value at the shocked spot, not a
	// linear 10%·delta move.
	down, ok := rv.RevalueOption(context.Background(), "OPT", asOf, baseMV, RevalShocks{PriceFrac: -0.10})
	if !ok {
		t.Fatal("expected option to be revalued")
	}
	wantDown := pricing.BlackScholesPrice(pricing.Call, spot*0.9, 100, ttm, 0, 0, sigma) * n
	if d := math.Abs(decimalToFloat(down.GetAmount()) - wantDown); d > 1.0 {
		t.Fatalf("reval down: got %.2f want %.2f", decimalToFloat(down.GetAmount()), wantDown)
	}

	// Vol +5 points raises the (long) option's value — the vega effect a linear
	// price shock cannot express.
	volUp, _ := rv.RevalueOption(context.Background(), "OPT", asOf, baseMV, RevalShocks{GlobalVolBump: 0.05})
	if decimalToFloat(volUp.GetAmount()) <= basePrice*n {
		t.Fatalf("vol bump should raise long-call value: got %.2f base %.2f", decimalToFloat(volUp.GetAmount()), basePrice*n)
	}

	// A non-option instrument is not revalued (linear fallback applies upstream).
	if _, ok := rv.RevalueOption(context.Background(), "EQ", asOf, baseMV, RevalShocks{PriceFrac: -0.1}); ok {
		t.Fatal("non-option instrument must not be revalued")
	}
}

// THE SCENARIO PATH REPORTS ITS DEGRADATIONS TOO (#509).
//
// RevalueOption falling back to the linear path is CORRECT here — a scenario must
// produce a shocked value for every position, and dropping one understates the
// loss. It is also a real degradation: an option shocked linearly carries no
// convexity. Silent, a scenario that linearised half the option book looked
// exactly like one that repriced it.
func TestRevalueOption_ReportsWhenItFallsBackToLinear(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	spec := OptionSpec{
		UnderlyingID: "UND", Strike: 100, Expiry: asOf.AddDate(1, 0, 0),
		Type: pricing.Call, Exercise: pricing.European, Multiplier: 1,
	}
	base := &commonpb.Money{Amount: dec(1000, 0), CurrencyCode: "USD"}

	cases := []struct {
		name      string
		providers GreeksProviders
		want      string
	}{
		{
			name: "no spot for the underlying",
			providers: GreeksProviders{
				Terms: staticTerms{"OPT": spec}, Spot: staticSpot{}, Vol: constVol(0.2),
			},
			want: SkipNoSpot,
		},
		{
			// A NIL PROVIDER USED TO PANIC HERE, on the same struct the measure
			// path guards — the scenario path reached it by a different route.
			name: "a nil spot provider",
			providers: GreeksProviders{
				Terms: staticTerms{"OPT": spec}, Spot: nil, Vol: constVol(0.2),
			},
			want: SkipNoSpot,
		},
		{
			name: "a nil vol provider",
			providers: GreeksProviders{
				Terms: staticTerms{"OPT": spec}, Spot: staticSpot{"UND": 100}, Vol: nil,
			},
			want: SkipNoVol,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			p := c.providers
			p.OnSkip = func(id, reason string) { got = append(got, id+":"+reason) }

			rv := NewRevaluer(p)
			if _, ok := rv.RevalueOption(context.Background(), "OPT", asOf, base, RevalShocks{PriceFrac: -0.1}); ok {
				t.Fatal("repriced despite a missing input — the fixture is not exercising the fallback")
			}
			if len(got) == 0 {
				t.Fatalf("fell back to the linear shock and reported nothing — an option shocked "+
					"linearly carries no convexity, and %s is invisible", c.want)
			}
			if got[0] != "OPT:"+c.want {
				t.Errorf("reported %q, want OPT:%s", got[0], c.want)
			}
		})
	}
}
