package compliance

import (
	"context"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// HOW MUCH CASH IS TOO MUCH? (#963)
//
// The mirror of buyingpower_test.go, and the tests that matter are the ones that
// do NOT mirror it. A floor and a ceiling on the same balance fail in opposite
// directions when the balance is understated, which is the state every book on
// this platform is in while #588 is open — so the ceiling refuses a balance the
// floor merely annotates, and the ceiling never gates an order at all.

func cashBufferRule(max *commonpb.Decimal) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "cb-1",
		Type:   compliancepb.RuleType_RULE_TYPE_CASH_BUFFER,
		Params: &compliancepb.Rule_CashBuffer{CashBuffer: &compliancepb.CashBufferLimit{MaxIdleCash: max}},
	}
}

// vouched is a completeness statement that names no omitted entry types — the
// producer saying "everything I fold is fed".
func vouched() *CashCompleteness { return &CashCompleteness{OmittedEntryTypes: []string{}} }

// bookWithVouchedCash is the monitor-shaped book: no Order, a cash balance its
// producer stands behind.
func bookWithVouchedCash(cash *commonpb.Money) *Book {
	b := bookWithCash(cash)
	b.CashCompleteness = vouched()
	return b
}

func TestCashBufferRule_AdmitsABookInsideItsCeiling(t *testing.T) {
	c := &Candidate{Book: bookWithVouchedCash(money(500, 0, "USD"))}
	if v := CashBufferRule(c, cashBufferRule(dec(1_000, 0))); v != nil {
		t.Fatalf("500 of cash under a 1000 ceiling was reported as a breach: %v", v)
	}
}

func TestCashBufferRule_AdmitsABookExactlyAtItsCeiling(t *testing.T) {
	// The bound is inclusive, matching BuyingPowerRule's floor: a mandate that
	// permits 1000 permits holding 1000.
	c := &Candidate{Book: bookWithVouchedCash(money(1_000, 0, "USD"))}
	if v := CashBufferRule(c, cashBufferRule(dec(1_000, 0))); v != nil {
		t.Fatalf("cash exactly AT the declared ceiling was reported as a breach: %v", v)
	}
}

func TestCashBufferRule_BreachesWhenIdleCashExceedsTheCeiling(t *testing.T) {
	c := &Candidate{Book: bookWithVouchedCash(money(2_500, 0, "USD"))}
	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("2500 of idle cash under a 1000 ceiling reported no breach — the drag this rule " +
			"exists to find is the only thing that would ever have reported it")
	}
	if got := v.GetEvidence()[EvidenceIdleCash]; ratFromString(t, got).Cmp(big.NewRat(2_500, 1)) != 0 {
		t.Errorf("evidence %s = %q, want 2500", EvidenceIdleCash, got)
	}
	if got := v.GetEvidence()[EvidenceIdleCashLimit]; ratFromString(t, got).Cmp(big.NewRat(1_000, 1)) != 0 {
		t.Errorf("evidence %s = %q, want 1000", EvidenceIdleCashLimit, got)
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "complete" {
		t.Errorf("a breach on a vouched balance recorded completeness %q, want %q — a reader of the "+
			"breach FACT cannot otherwise tell it from a finding that rests on a feed gap", got, "complete")
	}
}

// THE ONE THAT SEPARATES THE CEILING FROM THE FLOOR.
//
// BuyingPowerRule refuses on an unvouched balance and keeps its verdict, because
// an understated balance makes a FLOOR too strict. The same balance makes a
// CEILING too permissive: the dividends and coupons #588 keeps out of the book
// ARE idle cash, so the defect and the subject are the same money. A comparison
// here would report "no breach" and be recorded as a control that ran and
// approved.
func TestCashBufferRule_RefusesAnIncompleteBalanceRatherThanPassingIt(t *testing.T) {
	book := bookWithCash(money(500, 0, "USD"))
	book.CashCompleteness = &CashCompleteness{OmittedEntryTypes: []string{"corporate_action"}}
	c := &Candidate{Book: book}

	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("a balance the book of record says is INCOMPLETE was compared against the ceiling " +
			"and passed. The omitted entry types are dividends and coupons (#588), which is " +
			"exactly the cash an idle-cash ceiling is looking for — so the rule would report " +
			"clean on the books most likely to breach.")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "incomplete" {
		t.Errorf("evidence %s = %q, want %q", EvidenceBalanceCompleteness, got, "incomplete")
	}
	if got := v.GetEvidence()[EvidenceBalanceOmits]; got != "corporate_action" {
		t.Errorf("evidence %s = %q, want the omitted entry types", EvidenceBalanceOmits, got)
	}
}

func TestCashBufferRule_RefusesAnUnstatedBalanceRatherThanPassingIt(t *testing.T) {
	// nil completeness is UNSTATED, not complete: nobody wired the statement, or
	// the announcing accounting predates it. Either way no producer stands behind
	// the number.
	c := &Candidate{Book: bookWithCash(money(500, 0, "USD"))}
	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("a balance whose producer stated nothing about it was compared against the ceiling " +
			"and passed — 'nobody said' read as 'checked, and fine'")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "unstated" {
		t.Errorf("evidence %s = %q, want %q", EvidenceBalanceCompleteness, got, "unstated")
	}
}

// An UNVOUCHED balance that is ALSO over the ceiling must read as unverifiable,
// not as a breach: the missing entries move a SHORT book's cash the other way,
// so "apparently over" is not evidence of over.
func TestCashBufferRule_AnUnvouchedBalanceOverTheCeilingIsUnverifiableNotABreach(t *testing.T) {
	c := &Candidate{Book: bookWithCash(money(9_000, 0, "USD"))}
	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("an unvouched balance was admitted")
	}
	if got := v.GetEvidence()[EvidenceBalanceCompleteness]; got != "unstated" {
		t.Fatalf("evidence %s = %q: the comparison ran ahead of the completeness check, so a "+
			"breach FACT was published on a number nobody vouched for. One that turns out to "+
			"rest on a feed gap is how an operations desk learns to discount them.",
			EvidenceBalanceCompleteness, got)
	}
}

func TestCashBufferRule_RefusesWhenTheBalanceIsUnknown(t *testing.T) {
	book := bookWithVouchedCash(nil)
	c := &Candidate{Book: book}
	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("a portfolio with NO announced cash balance reported no idle-cash breach. Unknown " +
			"is not zero, and zero is the most compliant possible answer to this rule.")
	}
	if got := v.GetEvidence()[EvidenceIdleCash]; got != "unavailable" {
		t.Errorf("evidence %s = %q, want %q", EvidenceIdleCash, got, "unavailable")
	}
}

func TestCashBufferRule_RefusesAMandateThatDeclaresNoMaximum(t *testing.T) {
	c := &Candidate{Book: bookWithVouchedCash(money(500, 0, "USD"))}
	v := CashBufferRule(c, cashBufferRule(nil))
	if v == nil {
		t.Fatal("a cash-buffer rule with no max_idle_cash was treated as satisfied. A ceiling with " +
			"no number is a misconfigured mandate, and a misconfigured mandate fails closed.")
	}
	if got := v.GetEvidence()[EvidenceIdleCashLimit]; got != "unset" {
		t.Errorf("evidence %s = %q, want %q", EvidenceIdleCashLimit, got, "unset")
	}
}

// An EXPLICIT zero ceiling is a real policy and must still bind — proto3
// distinguishes it from the absent one above.
func TestCashBufferRule_AnExplicitZeroCeilingStillBinds(t *testing.T) {
	c := &Candidate{Book: bookWithVouchedCash(money(1, 0, "USD"))}
	if v := CashBufferRule(c, cashBufferRule(dec(0, 0))); v == nil {
		t.Fatal("an explicitly declared zero ceiling did not bind. It is a different mandate from " +
			"one that declares no ceiling at all, and collapsing the two loses the policy.")
	}
}

func TestCashBufferRule_RefusesCashOutsideTheBookBaseCurrency(t *testing.T) {
	book := bookWithVouchedCash(money(500, 0, "JPY"))
	c := &Candidate{Book: book} // BaseCurrency is USD
	v := CashBufferRule(c, cashBufferRule(dec(1_000, 0)))
	if v == nil {
		t.Fatal("500 JPY was compared against a ceiling denominated in USD and passed. The " +
			"comparison silently re-scales the LIMIT by the exchange rate, which is a policy " +
			"nobody declared.")
	}
	if got := v.GetEvidence()["base_currency"]; got != "USD" {
		t.Errorf("evidence base_currency = %q, want USD", got)
	}
}

// THE POSTURE, AND THE MOST IMPORTANT TEST HERE.
//
// A ceiling enforced at the pre-trade gate refuses the orders that discharge it
// and refuses a liquidation for raising cash. Every branch must stay off the
// order path — including the refusing ones, which is why the table runs the
// misconfigured and unverifiable cases too.
func TestCashBufferRule_NeverRefusesAnOrder(t *testing.T) {
	order := &CandidateOrder{InstrumentID: "AAPL", SignedQuantity: dec(-10, 0), Venue: "binance"}

	unvouched := bookWithCash(money(9_000, 0, "USD"))
	incomplete := bookWithCash(money(9_000, 0, "USD"))
	incomplete.CashCompleteness = &CashCompleteness{OmittedEntryTypes: []string{"corporate_action"}}

	cases := []struct {
		name string
		book *Book
		max  *commonpb.Decimal
	}{
		{"a sell that raises cash far above the ceiling", bookWithVouchedCash(money(9_000, 0, "USD")), dec(1_000, 0)},
		{"a buy out of a book already over its ceiling", bookWithVouchedCash(money(9_000, 0, "USD")), dec(1_000, 0)},
		{"a mandate whose ceiling is misconfigured", bookWithVouchedCash(money(9_000, 0, "USD")), nil},
		{"a book whose balance is unknown", bookWithVouchedCash(nil), dec(1_000, 0)},
		{"a book whose balance nobody vouched for", unvouched, dec(1_000, 0)},
		{"a book whose balance is incomplete", incomplete, dec(1_000, 0)},
		{"cash outside the book's base currency", bookWithVouchedCash(money(9_000, 0, "JPY")), dec(1_000, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Candidate{Book: tc.book, Order: order}
			if v := CashBufferRule(c, cashBufferRule(tc.max)); v != nil {
				t.Fatalf("the idle-cash ceiling refused an ORDER: %s\n\n"+
					"The remedy for excess cash is to BUY something, so a ceiling on the "+
					"admission path denies the orders that discharge it — and it denies a "+
					"SELL for raising cash, which traps the fund in a position at the moment "+
					"it most needs to leave (#963).", v.GetMessage())
			}
		})
	}
}

// The ONE refusal that does reach the order path, and the reason it is not an
// exception to the posture above: params that do not match the declared type is
// not a statement about cash, it is a mandate the engine cannot read, and a
// portfolio governed by an unreadable mandate is refused everywhere.
func TestCashBufferRule_DeniesAMismatchedParamsVariantOnBothPaths(t *testing.T) {
	rule := &compliancepb.Rule{RuleId: "cb-1", Type: compliancepb.RuleType_RULE_TYPE_CASH_BUFFER}
	for _, order := range []*CandidateOrder{nil, {InstrumentID: "AAPL", SignedQuantity: dec(-10, 0)}} {
		c := &Candidate{Book: bookWithVouchedCash(money(500, 0, "USD")), Order: order}
		if v := CashBufferRule(c, rule); v == nil {
			t.Fatalf("a RULE_TYPE_CASH_BUFFER carrying no cash_buffer params was treated as "+
				"satisfied (order=%v)", order != nil)
		}
	}
}

// THE AUTHORING SURFACE. Both publishers of a mandate — cmd/kanz-mandate and the
// compliance service's propose route — protojson.Unmarshal an operator's file
// into a compliance.v1.Mandate, so the ceiling is declarable only if the wire
// names resolve. A rule nobody can write down is reachable in the engine and
// unreachable in practice, which is the three-layer trap #539 cost a day to.
func TestCashBufferRule_IsAuthorableAsAMandateAnOperatorWrites(t *testing.T) {
	const authored = `{
	  "mandateId": "m1", "tenantId": "t1", "portfolioId": "p1", "version": 1,
	  "effectiveAt": "2026-01-01T00:00:00Z",
	  "rules": [{
	    "ruleId": "cb-1",
	    "type": "RULE_TYPE_CASH_BUFFER",
	    "cashBuffer": {"maxIdleCash": {"coefficient": "1000", "exponent": 0}}
	  }]
	}`
	var m compliancepb.Mandate
	if err := protojson.Unmarshal([]byte(authored), &m); err != nil {
		t.Fatalf("an operator-authored cash-buffer mandate does not parse: %v\n\n"+
			"Both publishers unmarshal exactly this way, so the ceiling cannot be declared at all.", err)
	}
	if err := ValidateMandate(&m); err != nil {
		t.Fatalf("ValidateMandate refused it: %v", err)
	}

	c := &Candidate{Book: bookWithVouchedCash(money(9_000, 0, "USD")), AsOf: t0}
	res := NewEngine(nil).Evaluate(context.Background(), c, &m)
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("the authored mandate evaluated to %s on a book holding 9000 against its declared "+
			"1000 ceiling, want BREACH", res.GetStatus())
	}
}

// THE WIRING. A rule the default registry does not carry is armed on neither the
// gate nor the monitor, and its absence is indistinguishable from a book that
// holds no idle cash.
func TestCashBufferRule_IsInTheDefaultRegistry(t *testing.T) {
	e := NewEngine(nil)
	c := &Candidate{Book: bookWithVouchedCash(money(9_000, 0, "USD")), AsOf: t0}
	res := e.Evaluate(context.Background(), c, mandate(cashBufferRule(dec(1_000, 0))))

	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("DefaultRegistry evaluated a cash-buffer mandate to %s, want BREACH", res.GetStatus())
	}
	if len(res.GetViolations()) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.GetViolations()))
	}
	// THE DENY-BY-DEFAULT ARM PRODUCES A BREACH TOO, so a status assertion alone
	// cannot tell "the rule ran and found the drag" from "no evaluator is
	// registered and the engine refused the rule it did not recognize". The
	// message is what separates them, and only one of the two is a control.
	got := res.GetViolations()[0]
	if got.GetMessage() == "unknown rule type — denied by default" {
		t.Fatal("RULE_TYPE_CASH_BUFFER has no evaluator in DefaultRegistry. Every mandate " +
			"declaring an idle-cash ceiling would breach on the deny-by-default arm instead of " +
			"being measured, on both the pre-trade gate and the passive sweep (#963).")
	}
	if e := got.GetEvidence()[EvidenceIdleCash]; e == "" {
		t.Errorf("the violation carries no %s evidence, so CashBufferRule did not produce it", EvidenceIdleCash)
	}
}
