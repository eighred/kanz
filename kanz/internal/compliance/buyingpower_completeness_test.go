package compliance

import (
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// A REFUSAL MUST SAY WHAT THE BALANCE WAS MISSING (#614).
//
// accounting's ledger folds six kinds of journal entry and NOTHING on this
// platform produces two of them (#588): no dividend, coupon or merger cash has
// ever reached the book, and no accrual has. So a portfolio that was paid a
// dividend is refused on a balance that does not contain it — and that refusal
// used to be spelled exactly like a mandate's spending limit being hit. Same
// message, same evidence, nothing in the audit trail to tell them apart.
//
// The three tests below pin the three answers apart. The one after them pins the
// property that matters more than any of them: NONE of this changes the verdict.

func bookWithCashAnd(cash *commonpb.Money, cc *CashCompleteness) *Book {
	b := bookWithCash(cash)
	b.CashCompleteness = cc
	return b
}

// UNSTATED IS NOT COMPLETE. A producer that said nothing has not shown its
// number to be whole — an accounting old enough to predate the statement, or a
// composition root that forgot to pass one. Reading that silence as a clean bill
// of health is the exact collapse this platform designs against.
func TestBuyingPowerRule_RefusalOnAnUnstatedBalanceSaysSo(t *testing.T) {
	c := &Candidate{Book: bookWithCashAnd(money(-250, 0, "USD"), nil)}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("an order that drives cash negative was admitted")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "unstated" {
		t.Errorf("evidence %s = %q, want unstated — a producer that said nothing must not read "+
			"as one that vouched for the number", EvidenceBalanceCompleteness, got)
	}
	if _, ok := v.GetEvidence()[EvidenceBalanceOmits]; ok {
		t.Errorf("evidence carries %s on an UNSTATED balance — nothing was named as missing, and "+
			"listing nothing would read as nothing being missing", EvidenceBalanceOmits)
	}
	if !strings.Contains(v.GetMessage(), "did not state") {
		t.Errorf("message = %q, want it to say the producer did not state what the balance "+
			"contains — otherwise this reads as a spending limit that was reached", v.GetMessage())
	}
}

// A BALANCE THE BOOK OF RECORD ADMITS IS INCOMPLETE MUST NOT REFUSE SILENTLY.
// This is the state every deployment is in today: corporate_action and accrual
// have no producer anywhere in the module.
func TestBuyingPowerRule_RefusalOnAnIncompleteBalanceNamesWhatIsMissing(t *testing.T) {
	c := &Candidate{Book: bookWithCashAnd(money(-250, 0, "USD"), &CashCompleteness{
		OmittedEntryTypes: []string{"accrual", "corporate_action"},
	})}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("an order that drives cash negative was admitted")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "incomplete" {
		t.Errorf("evidence %s = %q, want incomplete", EvidenceBalanceCompleteness, got)
	}
	if got := v.GetEvidence()[EvidenceBalanceOmits]; got != "accrual,corporate_action" {
		t.Errorf("evidence %s = %q, want accrual,corporate_action — a refusal that does not name "+
			"the missing feed sends a reader looking for a limit that was never reached",
			EvidenceBalanceOmits, got)
	}
	if !strings.Contains(v.GetMessage(), "INCOMPLETE") {
		t.Errorf("message = %q, want it to say the balance is INCOMPLETE", v.GetMessage())
	}
	// And it must not be spelled the same as a real limit breach, which is the
	// whole defect: "you are out of cash" and "we never counted your dividend"
	// were one sentence.
	plain := BuyingPowerRule(&Candidate{Book: bookWithCashAnd(money(-250, 0, "USD"),
		&CashCompleteness{})}, buyingPowerRule(nil))
	if v.GetMessage() == plain.GetMessage() {
		t.Errorf("a refusal on an INCOMPLETE balance is worded identically to one on a complete "+
			"balance (%q) — the two are indistinguishable in the audit trail, which is #614",
			v.GetMessage())
	}
}

// AND "CHECKED, AND FINE" MUST BE SAYABLE. A producer that states every entry
// type it folds is fed makes this a real spending limit, and a reader must be
// able to tell that from the two cases above without going and reading the
// producer's metrics.
func TestBuyingPowerRule_RefusalOnACompleteBalanceSaysItIsComplete(t *testing.T) {
	c := &Candidate{Book: bookWithCashAnd(money(-250, 0, "USD"), &CashCompleteness{})}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("an order that drives cash negative was admitted")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "complete" {
		t.Errorf("evidence %s = %q, want complete", EvidenceBalanceCompleteness, got)
	}
	if _, ok := v.GetEvidence()[EvidenceBalanceOmits]; ok {
		t.Errorf("evidence carries %s on a COMPLETE balance", EvidenceBalanceOmits)
	}
}

// COMPLETENESS CHANGES THE WORDS AND NEVER THE VERDICT (#614).
//
// This is the guard against the fix being worse than the bug. The tempting
// repair — treat an incomplete balance as understated and admit the order, or
// gross the number up by an assumed entitlement — turns a control that refuses
// too much into one that ADMITS WHAT THE FUND CANNOT PAY FOR. The direction of
// the error is not even knowable: accounting's foldCorpAct pays quantity x
// per-unit with the SIGN of the holding, so an unfolded dividend understates a
// long book's cash and OVERSTATES a short book's. Refusing is the only answer
// that is not a guess.
func TestBuyingPowerRule_CompletenessNeverChangesTheVerdict(t *testing.T) {
	postures := map[string]*CashCompleteness{
		"unstated":   nil,
		"complete":   {},
		"incomplete": {OmittedEntryTypes: []string{"corporate_action"}},
	}
	for name, cc := range postures {
		// Below the floor: refused under every posture.
		if v := BuyingPowerRule(&Candidate{Book: bookWithCashAnd(money(-250, 0, "USD"), cc)},
			buyingPowerRule(nil)); v == nil {
			t.Errorf("posture %q ADMITTED an order that drives cash to -250 against a zero floor — "+
				"a completeness statement must never buy an order more room than the number does",
				name)
		}
		// Above the floor: admitted under every posture. Without this arm, marking
		// a balance incomplete could quietly become a refusal of its own — which
		// would refuse every order on every deployment, since corporate_action is
		// unproduced in all of them.
		if v := BuyingPowerRule(&Candidate{Book: bookWithCashAnd(money(500, 0, "USD"), cc)},
			buyingPowerRule(nil)); v != nil {
			t.Errorf("posture %q REFUSED an affordable order (500 against a zero floor): %v — an "+
				"incomplete balance is a labelling problem, not a new denial", name, v.GetMessage())
		}
	}
}

// A BALANCE THAT IS NOT THERE AT ALL is still the unavailable case, not an
// incompleteness one. The operator's next action differs: wire the cash spine,
// rather than go and find a corporate-actions feed.
func TestBuyingPowerRule_UnavailableCashIsNotACompletenessProblem(t *testing.T) {
	c := &Candidate{Book: bookWithCashAnd(nil, &CashCompleteness{
		OmittedEntryTypes: []string{"corporate_action"},
	})}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("absent cash was admitted — it must fail closed")
	}
	if got := v.GetEvidence()["cash"]; got != "unavailable" {
		t.Errorf("evidence cash = %q, want unavailable", got)
	}
	if _, ok := v.GetEvidence()[EvidenceBalanceCompleteness]; ok {
		t.Errorf("an UNAVAILABLE balance was labelled with %s — there is no number to qualify, and "+
			"the fix is a cash spine rather than a corporate-actions feed",
			EvidenceBalanceCompleteness)
	}
}
