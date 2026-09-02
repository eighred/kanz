package treasury

// Cash-drag measurement tests (#963).
//
// Every test below names the WRONG NUMBER a treasury desk would otherwise have
// acted on. That framing matters here more than usual: a drag figure has no
// downstream control to contradict it — it breaches no mandate and trips no risk
// limit — so a plausible-but-wrong percentage survives all the way to somebody
// deciding what to do with the fund's cash.

import (
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
)

func money(s, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: dec.ToProto(dec.Rat(s)), CurrencyCode: ccy}
}

// vouchedBook is a book with both sides of the fraction trustworthy: equity NAV,
// a cash figure, and a producer that stated nothing is missing from it.
func vouchedBook(cash, nav string) *comp.Book {
	return &comp.Book{
		BaseCurrency:     "USD",
		Cash:             money(cash, "USD"),
		NAV:              money(nav, "USD"),
		NAVBasis:         comp.NAVBasisEquity,
		CashCompleteness: &comp.CashCompleteness{},
	}
}

// A BOOK WITH BOTH SIDES VOUCHED FOR GETS A FIGURE.
func TestAVouchedBookIsMeasured(t *testing.T) {
	got := Measure(vouchedBook("250000", "5000000"))
	if !got.Measured() {
		t.Fatalf("status = %s (%s: %s), want MEASURED", got.Status, got.Reason, got.Detail)
	}
	if want := big.NewRat(1, 20); got.IdleShare.Cmp(want) != 0 {
		t.Fatalf("idle share = %s, want %s — 250k of 5m equity is 5%%",
			got.IdleShare.RatString(), want.RatString())
	}
	if got.Reason != ReasonNone {
		t.Fatalf("a measured drag carries reason %q, want none", got.Reason)
	}
}

// THE ZERO VALUE OF Drag IS A REFUSAL, NOT A MEASURED 0%.
//
// Every refusal path below returns a Drag built by a literal that omits Status,
// so if StatusMeasured were the zero value each of them would report a portfolio
// as holding no idle cash — the exact false zero this package exists against.
func TestTheZeroDragIsUnknown(t *testing.T) {
	var d Drag
	if d.Measured() {
		t.Fatal("the zero Drag reads as MEASURED. Every refusal in Measure is built by a struct " +
			"literal that leaves Status unset, so this would turn all six of them into a " +
			"portfolio reported as holding 0% idle cash.")
	}
	if d.Percent() != 0 || d.IdleShare != nil {
		t.Fatal("the zero Drag carries a share")
	}
}

// Percent REFUSES ON STATUS, not on whether a share happens to be present.
//
// The zero-Drag test above cannot reach this: its IdleShare is nil, so a Percent
// that checked only the pointer would still return 0 and pass. The case that
// matters is a Drag carrying BOTH an UNKNOWN status and a share — which is what
// a caller assembling one by hand, or a future Measure that computed the ratio
// before deciding it could not be stated, would produce. Percent is documented
// as returning 0 on an UNKNOWN drag; if it honours only the pointer, that
// promise is the one thing standing between a refusal and a histogram bucket.
func TestPercentRefusesAnUnknownDragEvenWhenItCarriesAShare(t *testing.T) {
	d := Drag{Status: StatusUnknown, Reason: ReasonCashUnvouched, IdleShare: big.NewRat(1, 4)}
	if got := d.Percent(); got != 0 {
		t.Fatalf("Percent() = %v on an UNKNOWN drag carrying a share, want 0. Percent must refuse "+
			"on STATUS: a caller that trusts this contract would observe 25%% idle cash for a "+
			"book whose cash figure nobody vouched for.", got)
	}
}

// A CASH FIGURE NOBODY VOUCHED FOR IS NOT A NUMERATOR (#588/#614).
//
// This is the case that will be true for most of the estate today: accounting
// folds six entry types and two have no producer, so a book's cash is short every
// dividend and coupon ever paid to it. Reporting "3% idle" from that number is a
// treasury decision made on a figure that is wrong by an unknown amount, in a
// direction that depends on whether the book is long or short.
func TestAnUnvouchedCashBalanceRefuses(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.CashCompleteness = &comp.CashCompleteness{OmittedEntryTypes: []string{"corporate_action", "accrual"}}

	got := Measure(b)
	if got.Measured() {
		t.Fatalf("measured %s from a cash balance missing corporate actions and accruals — that "+
			"figure is short every dividend the fund has been paid", got.IdleShare.RatString())
	}
	if got.Reason != ReasonCashUnvouched {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonCashUnvouched)
	}
	for _, want := range []string{"corporate_action", "accrual", "SHORT"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("detail does not mention %q: %s\n\nThe omitted types name the feed to fix, and "+
				"the SHORT note is why the error's SIGN is not knowable — both are what makes "+
				"this refusal actionable rather than just a refusal.", want, got.Detail)
		}
	}
}

// UNSTATED IS NOT COMPLETE. A producer that said nothing has not shown its number
// to be whole, and collapsing that into "fine" is the defect comp.CashCompleteness
// exists to end. The two share a REASON here — nobody vouched — and must not share
// a DETAIL, because they send an operator to different places.
func TestAnUnstatedCompletenessRefusesAndSaysSoDifferently(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.CashCompleteness = nil

	got := Measure(b)
	if got.Measured() {
		t.Fatal("measured a drag from a cash balance whose producer stated no completeness — " +
			"nil is UNSTATED, not complete")
	}
	if got.Reason != ReasonCashUnvouched {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonCashUnvouched)
	}
	if strings.Contains(got.Detail, "omits entry types") {
		t.Fatalf("an UNSTATED balance was reported as an INCOMPLETE one: %s\n\n"+
			"Nobody wired the statement and a feed that does not exist are different problems "+
			"with different owners; the detail is the only place they stay apart.", got.Detail)
	}
	if !strings.Contains(got.Detail, "no completeness") {
		t.Fatalf("detail does not say the producer stated nothing: %s", got.Detail)
	}
}

// A GROSS-POSITIONS NAV IS THE WRONG DENOMINATOR (#780).
//
// The number exists and is plausible, which is what makes it dangerous. #780 is
// the precedent: gross and NAV were literally the same value, so a leverage ratio
// was structurally 1.0 and a cap bound on nothing. Cash over that is cash over
// the sum of position market values — no cash, no financing — and the resulting
// percentage is smaller than the truth for any book holding cash.
func TestAGrossPositionsNAVRefuses(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.NAVBasis = comp.NAVBasisGrossPositions

	got := Measure(b)
	if got.Measured() {
		t.Fatalf("measured %s against a gross-positions NAV — that denominator is not the fund's "+
			"value, and the figure understates the drag on every book holding cash",
			got.IdleShare.RatString())
	}
	if got.Reason != ReasonNAVNotEquity {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonNAVNotEquity)
	}
}

// AN UNSPECIFIED BASIS REFUSES TOO. "Nobody said what this NAV measures" and
// "this NAV is equity" must not collapse, which is why comp.NAVBasis has three
// values rather than a bool.
func TestAnUnspecifiedNAVBasisRefuses(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.NAVBasis = comp.NAVBasisUnspecified

	if got := Measure(b); got.Measured() || got.Reason != ReasonNAVNotEquity {
		t.Fatalf("status=%s reason=%q, want UNKNOWN/%s — an unspecified basis is not an assertion "+
			"of equity", got.Status, got.Reason, ReasonNAVNotEquity)
	}
}

// THE PRODUCER'S OWN NOTE SURVIVES INTO THE REFUSAL.
//
// comp.NAVBasisDetail exists because "this book's NAV is not equity" tells an
// operator nothing they can go and fix, while "no mark for SOL-USD" and "cash
// unknown" send them to two different teams. Dropping it here re-creates exactly
// the unactionable refusal that field was added to end.
func TestTheProducersNAVNoteReachesTheRefusal(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.NAV = nil
	b.NAVBasis = comp.NAVBasisUnspecified
	b.NAVBasisDetail = "no mark for SOL-USD"

	got := Measure(b)
	if got.Reason != ReasonNAVUnknown {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonNAVUnknown)
	}
	if !strings.Contains(got.Detail, "no mark for SOL-USD") {
		t.Fatalf("the producer's note was dropped: %s\n\nWithout it the refusal names no team "+
			"and no instrument, which is the state NAVBasisDetail was added to end.", got.Detail)
	}
}

// NO CASH ANNOUNCEMENT AT ALL IS ITS OWN REASON, distinct from an untrustworthy
// one: nothing has ever been announced for this portfolio, or the last
// announcement aged past cashview's bound. Both are a feed problem, not a
// completeness problem, and they route differently.
func TestABookWithNoCashRefusesWithItsOwnReason(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.Cash = nil

	got := Measure(b)
	if got.Reason != ReasonCashUnknown {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonCashUnknown)
	}
	if !strings.Contains(got.Detail, "accounting.balance.portfolio") {
		t.Fatalf("detail does not name the subject to check: %s", got.Detail)
	}
}

// THE NUMERATOR IS REPORTED FIRST when a book fails both sides.
//
// Deliberate, and the reason is estate-wide: #588 is one missing feed that
// changes every book at once, where a mark gap is usually one instrument. An
// operator reading the reason histogram should see the systemic cause dominate.
func TestABookFailingBothSidesReportsTheCashReason(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.CashCompleteness = nil
	b.NAV = nil
	b.NAVBasis = comp.NAVBasisUnspecified

	if got := Measure(b); got.Reason != ReasonCashUnvouched {
		t.Fatalf("reason = %q, want %q — the numerator is checked first", got.Reason, ReasonCashUnvouched)
	}
}

// A NON-POSITIVE DENOMINATOR IS REFUSED, NOT DIVIDED BY.
//
// big.Rat.Quo panics on zero, and a NEGATIVE equity would render a negative
// idle-cash percentage: an underwater book reported as the least idle on the
// estate, sorting to the top of a "well managed" dashboard.
func TestANonPositiveEquityRefuses(t *testing.T) {
	for _, nav := range []string{"0", "-1000000"} {
		b := vouchedBook("250000", nav)
		got := Measure(b)
		if got.Measured() {
			t.Fatalf("measured a drag against equity of %s", nav)
		}
		if got.Reason != ReasonNAVNotPositive {
			t.Fatalf("nav=%s reason = %q, want %q", nav, got.Reason, ReasonNAVNotPositive)
		}
	}
}

// CASH IN ANOTHER CURRENCY IS AN FX ERROR, NOT A RATIO.
func TestCashInAnotherCurrencyRefuses(t *testing.T) {
	b := vouchedBook("250000", "5000000")
	b.Cash = money("250000", "EUR")

	got := Measure(b)
	if got.Measured() {
		t.Fatalf("measured %s by dividing EUR cash into a USD book — an unconverted FX error "+
			"rendered as a percentage", got.IdleShare.RatString())
	}
	if got.Reason != ReasonCurrencyMismatch {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonCurrencyMismatch)
	}
}

// A NIL BOOK DOES NOT PANIC. This runs on a monitor sweep over every book the
// estate holds; a measurement that can take down the control loop it observes is
// a worse trade than a missing sample.
func TestANilBookIsUnknownAndDoesNotPanic(t *testing.T) {
	if got := Measure(nil); got.Measured() {
		t.Fatal("a nil book measured a drag")
	}
}

// EVERY REFUSAL REASON IS REACHABLE AND NAMED IN Reasons().
//
// A reason Measure can return but Reasons() omits ships with no metric series
// behind it, so the refusal it names is invisible on a dashboard — #806 and #803
// were both one hand-maintained enumeration missing a member.
func TestEveryReasonMeasureReturnsIsSeeded(t *testing.T) {
	seeded := map[Reason]bool{}
	for _, r := range Reasons() {
		seeded[r] = true
	}
	if seeded[ReasonNone] {
		t.Fatal("Reasons() includes ReasonNone — seeding the empty label puts a permanently-zero " +
			"\"\" series on the refusal metric")
	}

	// Each mutation drives Measure down one refusal path.
	paths := map[Reason]func(*comp.Book){
		ReasonCashUnknown:      func(b *comp.Book) { b.Cash = nil },
		ReasonCashUnvouched:    func(b *comp.Book) { b.CashCompleteness = nil },
		ReasonNAVUnknown:       func(b *comp.Book) { b.NAV = nil },
		ReasonNAVNotEquity:     func(b *comp.Book) { b.NAVBasis = comp.NAVBasisGrossPositions },
		ReasonNAVNotPositive:   func(b *comp.Book) { b.NAV = money("0", "USD") },
		ReasonCurrencyMismatch: func(b *comp.Book) { b.Cash = money("1", "EUR") },
	}
	for want, mutate := range paths {
		b := vouchedBook("250000", "5000000")
		mutate(b)
		if got := Measure(b).Reason; got != want {
			t.Fatalf("the path meant to produce %q produced %q — this test's premise has moved", want, got)
		}
		if !seeded[want] {
			t.Fatalf("Measure returns %q and Reasons() does not name it, so no metric series exists "+
				"for it and the refusal is invisible on a dashboard", want)
		}
	}
	if len(paths) != len(Reasons()) {
		t.Fatalf("Reasons() names %d refusals and %d are reachable here — either a reason became "+
			"unreachable (delete it) or a new one is untested", len(Reasons()), len(paths))
	}
}
