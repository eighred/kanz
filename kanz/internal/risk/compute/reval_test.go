package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/pricing"
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
