package compliance

import (
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// THE LEVERAGE DENOMINATOR HAS TO BE EQUITY (#780).
//
// LeverageRule divides gross exposure by Book.NAV. Every producer on this
// platform set NAV to the sum of position market values, and gross exposure is
// the sum of the ABSOLUTE values of the same positions — so for a book with no
// shorts the numerator and denominator were THE SAME NUMBER. The ratio was 1.0
// by construction, a max_gross_leverage of 1.5 could not bind however the fund
// was financed, and the check ran, passed, and was recorded in the audit trail
// as having approved the order.
//
// These tests are written against the two books the issue's acceptance criteria
// name — a long-only book that is genuinely levered, and a book that is mostly
// cash — and they assert the thing that was wrong: that those two books now get
// DIFFERENT answers where the proxy gave them the same one.

func leverageCap(limit int64, exp int32) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "lev-1",
		Type:   compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
		Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
			MaxGrossLeverage: &commonpb.Decimal{Coefficient: limit, Exponent: exp},
		}},
	}
}

// equityBook is a long-only book: holdings worth `positions`, financed by
// `equity`. A margin loan shows up as equity BELOW the holdings, which is what
// being levered means and what the old denominator could not see.
func equityBook(positions, equity int64) *Book {
	return &Book{
		PortfolioID: "p1", BaseCurrency: "USD",
		NAV:       money(equity, 0, "USD"),
		NAVBasis:  NAVBasisEquity,
		Positions: []Position{pos("AAPL", positions, 0, "USD")},
	}
}

// TestALongOnlyBookCanNowBreachALeverageCap is the issue's first acceptance
// criterion, and the regression test for the defect itself.
//
// $1.5m of holdings financed by $1m of equity is 1.5x gross leverage — a real,
// ordinary margin position. Under the positions-only denominator the same book
// scored 1,500,000/1,500,000 = 1.0 and passed every cap at or above 1.0,
// including this one. It is not a rounding difference; it is the control being
// unable to fire.
func TestALongOnlyBookCanNowBreachALeverageCap(t *testing.T) {
	c := &Candidate{Book: equityBook(1_500_000, 1_000_000)}

	v := LeverageRule(c, leverageCap(14, -1)) // cap 1.4x, actual 1.5x
	if v == nil {
		t.Fatal("a long-only book at 1.5x gross leverage passed a 1.4x cap. That is the #780 defect: " +
			"the denominator is the sum of the same positions the numerator sums, so the ratio is " +
			"1.0 for any book with no shorts and the cap cannot bind")
	}
	if got := v.GetEvidence()["observed"]; got != "1.500000" {
		t.Errorf("observed leverage = %q, want 1.500000 — the ratio must be gross/EQUITY", got)
	}

	// AND THE SAME BOOK PASSES A CAP IT IS ACTUALLY INSIDE. A rule that refused
	// everything would satisfy the assertion above and be useless.
	if v := LeverageRule(c, leverageCap(16, -1)); v != nil {
		t.Fatalf("a book at 1.5x breached a 1.6x cap: %s %v", v.GetMessage(), v.GetEvidence())
	}
}

// TestCashChangesTheAnswer is the issue's second acceptance criterion. A
// portfolio that is mostly cash and one that is fully invested produced the
// IDENTICAL ratio under the old denominator, because the cash a leverage limit
// is measured against was not in it.
//
// Both books below hold $100,000 of the same instrument. They differ only in
// how much uninvested cash sits behind them — which is the entire difference
// between a conservative book and a levered one.
func TestCashChangesTheAnswer(t *testing.T) {
	cases := []struct {
		name     string
		equity   int64
		wantLev  string
		breaches bool
	}{
		// $100k of holdings against $2m of equity: 0.05x. Nothing like a breach.
		{"mostly cash", 2_000_000, "0.050000", false},
		// The same holdings against $100k of equity: fully invested, 1.0x.
		{"fully invested", 100_000, "1.000000", false},
		// The same holdings against $50k of equity: half of it is borrowed, 2.0x.
		{"levered", 50_000, "2.000000", true},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Candidate{Book: equityBook(100_000, tc.equity)}
			v := LeverageRule(c, leverageCap(15, -1)) // 1.5x
			if tc.breaches && v == nil {
				t.Fatal("a book with more exposure than equity passed a 1.5x cap")
			}
			if !tc.breaches && v != nil {
				t.Fatalf("unexpected violation: %s %v", v.GetMessage(), v.GetEvidence())
			}
			// Read the ratio back whichever way the verdict went, so the passing
			// cases are pinned to a NUMBER and not merely to "no violation" — which
			// is what let the 1.0-for-everything denominator survive.
			got := observedLeverage(t, c)
			if got != tc.wantLev {
				t.Errorf("observed leverage = %q, want %q", got, tc.wantLev)
			}
			seen[tc.name] = got
		})
	}
	// THE POINT OF THE TABLE. Under the positions-only denominator all three of
	// these were "1.000000", so a test asserting each one individually could pass
	// while the control was inert. They must be three different numbers.
	if seen["mostly cash"] == seen["fully invested"] || seen["fully invested"] == seen["levered"] {
		t.Fatalf("books with different financing produced the same leverage: %v — the denominator "+
			"is not responding to equity", seen)
	}
}

// observedLeverage reads the ratio the rule computes, by asking for a cap of
// zero so the violation always carries it.
func observedLeverage(t *testing.T, c *Candidate) string {
	t.Helper()
	v := LeverageRule(c, leverageCap(0, 0))
	if v == nil {
		t.Fatal("a zero cap admitted a book with exposure — nothing can be inside a 0x cap")
	}
	return v.GetEvidence()["observed"]
}

// TestAProxyDenominatorIsRefusedRatherThanDividedBy covers the three ways the
// denominator can fail to be equity. Each is a separate refusal because the
// operator's next action differs, and a single message would send them to the
// wrong place.
func TestAProxyDenominatorIsRefusedRatherThanDividedBy(t *testing.T) {
	cases := []struct {
		name      string
		book      *Book
		wantBasis string
		wantMsg   string
	}{
		{
			// Nobody said. This is what a Book literal gets for free, and it is the
			// reason the zero value has to be the refusing one.
			name: "unspecified",
			book: &Book{PortfolioID: "p1", BaseCurrency: "USD",
				NAV:       money(1_000_000, 0, "USD"),
				Positions: []Position{pos("AAPL", 1_500_000, 0, "USD")}},
			wantBasis: "unspecified",
			wantMsg:   "not an equity figure",
		},
		{
			// A producer said, honestly, that its number is a positions total.
			name: "gross positions",
			book: &Book{PortfolioID: "p1", BaseCurrency: "USD",
				NAV:       money(1_500_000, 0, "USD"),
				NAVBasis:  NAVBasisGrossPositions,
				Positions: []Position{pos("AAPL", 1_500_000, 0, "USD")}},
			wantBasis: "gross_positions",
			wantMsg:   "not an equity figure",
		},
		{
			// The book asserts equity and has none to report.
			name: "equity asserted but absent",
			book: &Book{PortfolioID: "p1", BaseCurrency: "USD",
				NAVBasis:  NAVBasisEquity,
				Positions: []Position{pos("AAPL", 1_500_000, 0, "USD")}},
			wantBasis: "equity",
			wantMsg:   "NAV unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := LeverageRule(&Candidate{Book: tc.book}, leverageCap(15, -1))
			if v == nil {
				t.Fatal("a leverage cap was evaluated against a denominator that is not equity, and " +
					"the book passed. A control that divides by a placeholder reports success")
			}
			if !strings.Contains(v.GetMessage(), tc.wantMsg) {
				t.Errorf("message %q does not contain %q", v.GetMessage(), tc.wantMsg)
			}
			if got := v.GetEvidence()[EvidenceNAVBasis]; got != tc.wantBasis {
				t.Errorf("%s = %q, want %q — the refusal must say which of the three states the "+
					"book was in", EvidenceNAVBasis, got, tc.wantBasis)
			}
		})
	}
}

// TestTheProducersReasonReachesTheRefusal pins the one thing that makes these
// refusals actionable. "This book's NAV is not equity" is true of a deployment
// that never wired a price feed AND of one whose feed stalled on a single
// instrument, and those are different call-outs.
func TestTheProducersReasonReachesTheRefusal(t *testing.T) {
	b := &Book{PortfolioID: "p1", BaseCurrency: "USD",
		NAV:            money(1_500_000, 0, "USD"),
		NAVBasis:       NAVBasisGrossPositions,
		NAVBasisDetail: "no live mark for SOL-USD",
		Positions:      []Position{pos("AAPL", 1_500_000, 0, "USD")}}

	v := LeverageRule(&Candidate{Book: b}, leverageCap(15, -1))
	if v == nil {
		t.Fatal("a proxy denominator was accepted")
	}
	if got := v.GetEvidence()[EvidenceNAVBasisDetail]; got != "no live mark for SOL-USD" {
		t.Fatalf("%s = %q, want the producer's reason. Without it an operator learns only that "+
			"leverage is unverifiable, which does not tell them where to go",
			EvidenceNAVBasisDetail, got)
	}

	// AND IT IS ABSENT WHEN NOBODY SET IT, rather than an empty key in the audit
	// evidence that reads as "the producer had no reason".
	b.NAVBasisDetail = ""
	v = LeverageRule(&Candidate{Book: b}, leverageCap(15, -1))
	if _, present := v.GetEvidence()[EvidenceNAVBasisDetail]; present {
		t.Fatalf("%s is present with no detail set: %v", EvidenceNAVBasisDetail, v.GetEvidence())
	}
}

// TestNonPositiveEquityIsUndefinedLeverageNotComplianceCloses the second
// fail-open, and it is the subtler of the two.
//
// The denominator used to be read through absRatFromMoney, which takes the
// ABSOLUTE value. Equity of -$1,000,000 became a denominator of $1,000,000, so a
// book that had been wiped out — liabilities exceeding assets — reported
// leverage of exactly 1.0 and passed a 1.5x cap. Only the ZERO case was caught,
// because zero is the one value whose absolute is still zero.
//
// A fund whose equity is zero or negative has leverage that is undefined or
// infinite. It is never "within cap".
func TestNonPositiveEquityIsUndefinedLeverageNotCompliance(t *testing.T) {
	for _, equity := range []int64{0, -1_000_000} {
		c := &Candidate{Book: equityBook(1_000_000, equity)}
		v := LeverageRule(c, leverageCap(15, -1))
		if v == nil {
			t.Fatalf("equity %d: a book with non-positive equity passed a 1.5x cap. Taking the "+
				"absolute value of the denominator turns a wiped-out book into a compliant one",
				equity)
		}
		if got := v.GetEvidence()[EvidenceNAVBasis]; got != "equity" {
			t.Errorf("equity %d: %s = %q, want \"equity\" — the book DID assert equity; what is "+
				"wrong is the number, not the basis", equity, EvidenceNAVBasis, got)
		}
	}
}

// TestAnUnmarkedHoldingStillWinsOverTheEquityCheck pins the ORDER of the two
// refusals, which is not arbitrary.
//
// #760's unmarked-holdings refusal exists because gross is a SUM: a holding
// nobody could price shrinks the numerator, so the book reads as less levered
// for carrying it. That refusal must survive the equity check being added in
// front of it — a book that is both unmarked and equity-backed has to be refused
// for the reason that is actionable, and #760's names the instrument.
// fixedMarks prices every instrument the same, so a test can drive
// equityFromMarks without a price feed.
type fixedMarks struct{ px *big.Rat }

func (m fixedMarks) Mark(string) *big.Rat { return m.px }

// EQUITY IS NOT SUMMED OVER A MARK WITH NO UNIT (#806).
//
// equityFromMarks states its assumption openly — the mark is in the portfolio's
// base currency — and refuses a book whose positions are demonstrably in another
// one. The check was `mv.GetCurrencyCode() != "" && != BaseCurrency`, so a
// holding whose currency NOBODY STATED skipped it and was valued at the
// base-currency mark anyway.
//
// That is the same fail-open shape as #806's CurrencyRule, two functions over,
// and it lands on the leverage denominator: a book reads as LESS levered for
// carrying a position nobody stated the currency of. "Nothing configured" and
// "checked, and fine" must never look the same.
func TestEquity_AMarkWithNoCurrencyIsNotAssumedToBeBaseCurrency(t *testing.T) {
	b := &Book{
		PortfolioID: "p1", BaseCurrency: "USD",
		Cash: money(1_000_000, 0, "USD"),
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     dec(1000, 0),
			// An amount, and no currency code: priced, unitless.
			MarketValue: &commonpb.Money{Amount: dec(500_000, 0)},
		}},
	}
	MarkEquity(b, fixedMarks{px: big.NewRat(500, 1)})

	if b.NAVBasis == NAVBasisEquity {
		t.Fatalf("a position carrying an amount with no currency code was summed into equity as "+
			"though it were %s — the assumption equityFromMarks documents was never checked for "+
			"this holding, and equity is LeverageRule's denominator: NAV=%v basis=%q",
			b.BaseCurrency, b.NAV, b.NAVBasis)
	}
	if !strings.Contains(b.NAVBasisDetail, "no currency code") {
		t.Errorf("NAVBasisDetail=%q must name the absence it found, so an operator knows to go "+
			"and fix the producer rather than the price feed", b.NAVBasisDetail)
	}
}

// NON-VACUITY. The same book with the currency STATED must still compute equity —
// otherwise the refusal above is just a broken equity path.
func TestEquity_AStatedBaseCurrencyStillComputesEquity(t *testing.T) {
	b := &Book{
		PortfolioID: "p1", BaseCurrency: "USD",
		Cash: money(1_000_000, 0, "USD"),
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     dec(1000, 0),
			MarketValue:  money(500_000, 0, "USD"),
		}},
	}
	MarkEquity(b, fixedMarks{px: big.NewRat(500, 1)})

	if b.NAVBasis != NAVBasisEquity {
		t.Fatalf("a fully stated book did not produce equity: basis=%q detail=%q",
			b.NAVBasis, b.NAVBasisDetail)
	}
}

// AND A HOLDING THE BOOK NEVER PRICED IS STILL NOT THAT. A nil MarketValue, or
// one with no amount, is a holding awaiting a mark — which is exactly what
// equityFromMarks is for — and must not be caught by the refusal above.
func TestEquity_AnUnpricedHoldingIsStillValuedFromItsMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		mv   *commonpb.Money
	}{
		{"no market value", nil},
		{"a market value with no amount", &commonpb.Money{CurrencyCode: "USD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Book{
				PortfolioID: "p1", BaseCurrency: "USD",
				Cash:      money(1_000_000, 0, "USD"),
				Positions: []Position{{InstrumentID: "AAPL", Quantity: dec(1000, 0), MarketValue: tc.mv}},
			}
			MarkEquity(b, fixedMarks{px: big.NewRat(500, 1)})
			if b.NAVBasis != NAVBasisEquity {
				t.Fatalf("a holding awaiting a mark was refused instead of valued from one — "+
					"that is what equityFromMarks exists to do: basis=%q detail=%q",
					b.NAVBasis, b.NAVBasisDetail)
			}
		})
	}
}

func TestAnUnmarkedHoldingStillWinsOverTheEquityCheck(t *testing.T) {
	b := equityBook(1_000_000, 1_000_000)
	b.Positions = append(b.Positions, Position{InstrumentID: "SOL-USD"}) // no MarketValue

	v := LeverageRule(&Candidate{Book: b}, leverageCap(15, -1))
	if v == nil {
		t.Fatal("a book carrying a position nobody could price was admitted")
	}
	if got := v.GetEvidence()[EvidenceUnmarkedSample]; got != "SOL-USD" {
		t.Fatalf("%s = %q, want SOL-USD — the equity check must not swallow #760's refusal, which "+
			"is the one that names the instrument an operator has to go and price",
			EvidenceUnmarkedSample, got)
	}
}
