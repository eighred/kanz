package accounting

import (
	"math/big"
	"testing"

	"github.com/kanz-eng/kanz/services/accounting/internal/dec"
	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

// A EUR cash entry: signed cash leg in EUR (mirrors nav_test's cash() but foreign).
func cashCcy(id, amt, ccy string, eff int) *ledger.Event {
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryCash,
		Cash: dec.Rat(amt), CashCurrency: ccy, Effective: day(eff), Knowledge: day(eff),
	}
}

func buyCcy(id, inst, qty, price, ccy string, eff int) *ledger.Event {
	q := dec.Rat(qty)
	p := dec.Rat(price)
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: inst,
		Quantity: q, Price: p, Cash: new(big.Rat).Neg(new(big.Rat).Mul(q, p)), CashCurrency: ccy,
		Effective: day(eff), Knowledge: day(eff),
	}
}

// A single-currency book with an identity FX table reproduces ComputeNAV exactly
// — the multi-currency path is the general form the domestic path is a case of.
func TestComputeNAVInCurrency_DomesticMatchesComputeNAV(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{cash("c1", "100000", 1), buy("t1", "AAPL", "100", "150", 2)})
	prices := map[string]*big.Rat{"AAPL": dec.Rat("160")}

	want, err := ComputeNAV(b, "USD", day(2), prices)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ComputeNAVInCurrency(b, "USD", day(2), prices, nil, NewFXTable("USD", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.Cmp(want.Total) != 0 || got.Cash.Cmp(want.Cash) != 0 ||
		got.SecurityValue.Cmp(want.SecurityValue) != 0 {
		t.Fatalf("multi-ccy domestic NAV = (total %s cash %s sec %s) want (%s %s %s)",
			got.Total.RatString(), got.Cash.RatString(), got.SecurityValue.RatString(),
			want.Total.RatString(), want.Cash.RatString(), want.SecurityValue.RatString())
	}
}

// A book with a EUR-priced holding + EUR cash values into USD at the given rate,
// and the per-instrument reference-currency join converts the holding's price.
func TestComputeNAVInCurrency_ConvertsForeignHoldingAndCash(t *testing.T) {
	// Buy 100 VOD @ €80 (pay €8000), leaving €2000 of a €10000 EUR cash seed.
	b := ledger.Replay("PF", []*ledger.Event{
		cashCcy("c1", "10000", "EUR", 1),
		buyCcy("t1", "VOD", "100", "80", "EUR", 2),
	})
	instrCcy := InstrumentCurrency{"VOD": "EUR"}
	prices := map[string]*big.Rat{"VOD": dec.Rat("85")} // €85 mark
	fx := NewFXTable("USD", map[string]*big.Rat{"EUR": dec.Rat("1.10")})

	nav, err := ComputeNAVInCurrency(b, "USD", day(2), prices, instrCcy, fx)
	if err != nil {
		t.Fatal(err)
	}
	// sec local = 100×85 = €8500 → ×1.10 = $9350
	if nav.SecurityValue.Cmp(dec.Rat("9350")) != 0 {
		t.Fatalf("sec = %s want 9350", nav.SecurityValue.RatString())
	}
	// cash local = €2000 → ×1.10 = $2200
	if nav.Cash.Cmp(dec.Rat("2200")) != 0 {
		t.Fatalf("cash = %s want 2200", nav.Cash.RatString())
	}
	if nav.Total.Cmp(dec.Rat("11550")) != 0 {
		t.Fatalf("total = %s want 11550", nav.Total.RatString())
	}
	// LocalExposure carries the pre-conversion EUR book value (€8500 + €2000).
	if nav.LocalExposure["EUR"].Cmp(dec.Rat("10500")) != 0 {
		t.Fatalf("EUR local exposure = %s want 10500", nav.LocalExposure["EUR"].RatString())
	}
}

// A missing FX rate for a currency the book actually holds fails the valuation.
func TestComputeNAVInCurrency_MissingRateErrors(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{cashCcy("c1", "5000", "JPY", 1)})
	fx := NewFXTable("USD", nil) // no JPY rate
	if _, err := ComputeNAVInCurrency(b, "USD", day(1), nil, nil, fx); err == nil {
		t.Fatal("expected an error for a missing FX rate")
	}
}

// The fx attribution driver is the prior exposure revalued at the rate change:
// €10,500 held × (1.20 − 1.10) = $1,050. The reporting currency contributes 0.
func TestFXPnL_RevaluesPriorExposureAtRateChange(t *testing.T) {
	exposure := map[string]*big.Rat{"EUR": dec.Rat("10500"), "USD": dec.Rat("2000")}
	priorFX := NewFXTable("USD", map[string]*big.Rat{"EUR": dec.Rat("1.10")})
	currentFX := NewFXTable("USD", map[string]*big.Rat{"EUR": dec.Rat("1.20")})

	fx := FXPnL(exposure, priorFX, currentFX)
	if fx.Cmp(dec.Rat("1050")) != 0 {
		t.Fatalf("fx pnl = %s want 1050", fx.RatString())
	}
}

// Attribute consumes the real-rate fx driver and the identity still holds:
// price + cash + fx + corporate_action == ΔTotal.
func TestAttribute_WithRealFXPreservesIdentity(t *testing.T) {
	// Prior: 100 VOD @ €80 mark, €2000 cash, EUR@1.10. Current: €90 mark, EUR@1.20.
	b := ledger.Replay("PF", []*ledger.Event{
		cashCcy("c1", "10000", "EUR", 1),
		buyCcy("t1", "VOD", "100", "80", "EUR", 2),
	})
	instrCcy := InstrumentCurrency{"VOD": "EUR"}
	priorFX := NewFXTable("USD", map[string]*big.Rat{"EUR": dec.Rat("1.10")})
	currentFX := NewFXTable("USD", map[string]*big.Rat{"EUR": dec.Rat("1.20")})

	prior, err := ComputeNAVInCurrency(b, "USD", day(2), map[string]*big.Rat{"VOD": dec.Rat("80")}, instrCcy, priorFX)
	if err != nil {
		t.Fatal(err)
	}
	current, err := ComputeNAVInCurrency(b, "USD", day(3), map[string]*big.Rat{"VOD": dec.Rat("90")}, instrCcy, currentFX)
	if err != nil {
		t.Fatal(err)
	}

	fx := FXPnL(prior.LocalExposure, priorFX, currentFX)
	current = Attribute(prior, current, nil, fx)

	deltaTotal := new(big.Rat).Sub(current.Total, prior.Total)
	if got := AttributionTotal(current); got.Cmp(deltaTotal) != 0 {
		t.Fatalf("attribution sum %s != ΔTotal %s", got.RatString(), deltaTotal.RatString())
	}
	// fx component is populated from the real rate move, not zero.
	var fxComp *big.Rat
	for _, c := range current.Attribution {
		if c.Source == "fx" {
			fxComp = c.Amount
		}
	}
	if fxComp == nil || fxComp.Sign() == 0 {
		t.Fatalf("fx attribution component is zero/absent: %v", fxComp)
	}
}
