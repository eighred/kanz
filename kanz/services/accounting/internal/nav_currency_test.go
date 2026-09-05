package accounting

import (
	"math/big"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// A NAV OVER A BOOK IT CANNOT VALUE MUST REFUSE, NOT UNDERSTATE (#1041).
//
// ComputeNAV read ONE cash bucket and ONE accrued bucket, so a book holding
// USD 100 and EUR 50 answered NAV{Cash: 100}, err = nil: the EUR was not summed,
// not converted, and not reported as omitted. The error was the whole value of
// every non-reporting currency the fund held, in the direction of "smaller than
// it is", on the fund's headline number.
//
// These are the cases the package had none of before: every ComputeNAV test was
// single-currency, so the assumption in its doc comment was never exercised.

func cashIn(id, amt, ccy string, eff int) *ledger.Event {
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryCash,
		Cash: dec.Rat(amt), CashCurrency: ccy, Effective: day(eff), Knowledge: day(eff),
	}
}

// THE ISSUE'S OWN ACCEPTANCE CASE: USD 100 + EUR 50, valued in USD.
func TestComputeNAVRefusesABookHoldingForeignCash(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{
		cashIn("c-usd", "100", "USD", 1),
		cashIn("c-eur", "50", "EUR", 1),
	})

	nav, err := ComputeNAV(b, "USD", day(2), map[string]*big.Rat{})
	if err == nil {
		t.Fatalf("ComputeNAV over a USD+EUR book returned NAV{Cash: %s, Total: %s} and NO ERROR. "+
			"The EUR 50 was dropped: the fund's headline number came back understated by the whole "+
			"value of every non-reporting currency it holds, and nothing said so (#1041)",
			nav.Cash.RatString(), nav.Total.RatString())
	}
	// AND IT MUST NAME THE CURRENCY. "cannot value this book" sends an operator
	// looking; "EUR" tells them which rate to configure.
	if !strings.Contains(err.Error(), "EUR") {
		t.Fatalf("the refusal does not name the currency it cannot value: %v", err)
	}
}

// The accrued bucket is the same map with the same single-entry reader, and
// earned-not-received income in a foreign currency is exactly as droppable.
func TestComputeNAVRefusesABookHoldingForeignAccrued(t *testing.T) {
	b := ledger.NewBook("PF")
	b.Apply(cashIn("c-usd", "100", "USD", 1))
	b.Apply(AccrualEntry("acc-gbp", "PF", "GILT", "GBP", dec.Rat("25"), day(1), day(1)))

	if _, err := ComputeNAV(b, "USD", day(2), map[string]*big.Rat{}); err == nil {
		t.Fatal("ComputeNAV over a book accruing GBP income returned no error — the accrued " +
			"bucket is read one currency at a time exactly as cash is, so the income is dropped " +
			"from NAV with nothing reported (#1041)")
	} else if !strings.Contains(err.Error(), "GBP") {
		t.Fatalf("the refusal does not name GBP: %v", err)
	}
}

// A ZERO BUCKET IS NOT A HOLDING. A book that traded out of EUR keeps the key
// with a zero balance; refusing on that would take every such portfolio's NAV
// offline for a currency it does not hold.
func TestComputeNAVValuesABookWhoseForeignBucketIsFlat(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{
		cashIn("c-usd", "100", "USD", 1),
		cashIn("c-eur-in", "50", "EUR", 1),
		cashIn("c-eur-out", "-50", "EUR", 2),
	})
	if _, ok := b.Cash["EUR"]; !ok {
		t.Fatal("this test is vacuous: the EUR bucket is absent, not zero")
	}
	nav, err := ComputeNAV(b, "USD", day(3), map[string]*big.Rat{})
	if err != nil {
		t.Fatalf("a flat EUR bucket must not refuse: %v", err)
	}
	if nav.Total.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("total = %s want 100", nav.Total.RatString())
	}
}

// THE DOMESTIC NUMBERS DO NOT MOVE. ComputeNAV is now ComputeNAVInCurrency with
// an identity rate table, which is what its doc comment always claimed the
// relationship was; this pins the claim from the ComputeNAV side so a future
// divergence is a failing test rather than a quietly different NAV.
func TestComputeNAVMatchesTheGeneralFormOnADomesticBook(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{cash("c1", "100000", 1), buy("t1", "AAPL", "100", "150", 2)})
	prices := map[string]*big.Rat{"AAPL": dec.Rat("160")}

	fast, err := ComputeNAV(b, "USD", day(2), prices)
	if err != nil {
		t.Fatal(err)
	}
	general, err := ComputeNAVInCurrency(b, "USD", day(2), prices, nil, NewFXTable("USD", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		fast, gen *big.Rat
	}{
		{"total", fast.Total, general.Total},
		{"cash", fast.Cash, general.Cash},
		{"security_value", fast.SecurityValue, general.SecurityValue},
		{"accrued", fast.Accrued, general.Accrued},
	} {
		if c.fast.Cmp(c.gen) != 0 {
			t.Errorf("%s: ComputeNAV = %s, ComputeNAVInCurrency = %s — the two NAV forms disagree "+
				"on a domestic book, which is the divergence #1041 was", c.name, c.fast.RatString(), c.gen.RatString())
		}
	}
}
