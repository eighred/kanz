package compliance

import (
	"context"
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// AN ADMISSION AGAINST A BALANCE NOBODY VOUCHED FOR (#671).
//
// #614 made a REFUSAL say what the balance it refused on could not account for.
// The pass path said nothing, and the pass path is the dangerous direction:
// foldCorpAct pays quantity × per-unit with the SIGN of the holding, so an
// unfolded dividend OVERSTATES a short book's cash. The order admitted on that
// number is one the fund may not be able to pay for.
//
// The verdict must not move — corporate_action is unproduced in EVERY deployment
// (#588), so refusing on it would be a trading outage rather than a control. What
// changes is that the admission is now attributable.

// bookWithCompleteness returns the standard test book with a stated completeness posture.
// A nil cc is the UNSTATED case: nobody said anything.
func bookWithCompleteness(cc *CashCompleteness) *Book {
	b := currentBook()
	b.CashCompleteness = cc
	return b
}

// gateOverBook builds a gate over one book, recording every unaccounted
// admission the gate observes.
func gateOverBook(t *testing.T, m *compliancepb.Mandate, b *Book) (*PreTradeGate, *[]string) {
	t.Helper()
	reg := NewMandateRegistry()
	if m != nil {
		mustPut(t, reg, m)
	}
	seen := &[]string{}
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": b}, reg, nil, nil, nil,
		WithUnaccountedObserver(func(_, _, omits string) { *seen = append(*seen, omits) }),
	)
	return g, seen
}

// passingOrder buys AAPL into a 90% cap: 200k of 300k is 66.7%, which passes.
func passingOrder() OrderDelta {
	return OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o1", AsOf: t0,
	}
}

func TestAnAdmissionAgainstAnIncompleteBalanceIsObserved(t *testing.T) {
	g, seen := gateOverBook(t, concentrationMandate(90),
		bookWithCompleteness(&CashCompleteness{OmittedEntryTypes: []string{"accrual", "corporate_action"}}))

	got, err := g.Evaluate(context.Background(), passingOrder())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("the order must still be ADMITTED — completeness does not move the verdict (#614). got %+v", got)
	}
	if !got.Unaccounted {
		t.Fatal("Decision.Unaccounted is false for an admission against a balance missing accrual and " +
			"corporate_action — the caller cannot tell this pass from one against a whole balance")
	}
	if len(*seen) != 1 || (*seen)[0] != "accrual,corporate_action" {
		t.Fatalf("observer saw %v, want exactly one call naming the omitted entry types — the counter "+
			"is how 'orders cleared against a number nobody stands behind' becomes visible", *seen)
	}
}

func TestAnAdmissionAgainstAnUnstatedBalanceIsObserved(t *testing.T) {
	// nil: the producer said nothing at all. Distinct from incomplete, and a
	// DIFFERENT operator action — the announcer is not declaring completeness,
	// which is a wiring fault fixable today rather than a missing feed.
	g, seen := gateOverBook(t, concentrationMandate(90), bookWithCompleteness(nil))

	got, err := g.Evaluate(context.Background(), passingOrder())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed || !got.Unaccounted {
		t.Fatalf("an unstated balance must admit AND be flagged, got allowed=%v unaccounted=%v",
			got.Allowed, got.Unaccounted)
	}
	if len(*seen) != 1 || (*seen)[0] != "" {
		t.Fatalf("observer saw %v, want one call with an empty omits string — 'nobody said' must not "+
			"be reported as though a feed had been named", *seen)
	}
}

func TestAnAdmissionAgainstAVouchedBalanceIsNotObserved(t *testing.T) {
	// Stated, and nothing missing. This is the only posture that may pass quietly.
	g, seen := gateOverBook(t, concentrationMandate(90), bookWithCompleteness(&CashCompleteness{}))

	got, err := g.Evaluate(context.Background(), passingOrder())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("want admitted, got %+v", got)
	}
	if got.Unaccounted {
		t.Fatal("Decision.Unaccounted is true for a balance the producer vouched for — if a complete " +
			"balance also raises this flag, the flag carries no information")
	}
	if len(*seen) != 0 {
		t.Fatalf("observer fired %v times on a vouched balance — a counter that increments on every "+
			"order is a counter nobody will look at", *seen)
	}
}

func TestARefusalDoesNotAlsoReportTheBalance(t *testing.T) {
	// A 60% cap: buying 100 AAPL takes it to 66.7% and BREACHES.
	//
	// The refusal path already carries the statement as EVIDENCE — attributeCash
	// puts balance_completeness and balance_omits on the violation (#614). Firing
	// the observer here as well would report one fact in two shapes and inflate a
	// metric whose name says "admissions".
	g, seen := gateOverBook(t, concentrationMandate(60),
		bookWithCompleteness(&CashCompleteness{OmittedEntryTypes: []string{"corporate_action"}}))

	got, err := g.Evaluate(context.Background(), passingOrder())
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatalf("this order must breach the 60%% cap; the case tests the REFUSAL path. got %+v", got)
	}
	if got.Unaccounted {
		t.Fatal("Decision.Unaccounted is set on a REFUSAL — it means 'admitted against an unvouched " +
			"balance', and a refusal was not admitted")
	}
	if len(*seen) != 0 {
		t.Fatalf("observer fired %v on a refusal — the violation already carries balance_completeness "+
			"and balance_omits, so this double-reports and miscounts the admissions metric", *seen)
	}
}

func TestEveryUnaccountedAdmissionIsCountedNotJustTheFirst(t *testing.T) {
	// COUNT ALWAYS, SAY ONCE. The WARN is rate-limited per portfolio so it does not
	// drown the log for a fund that trades all day; the OBSERVER is not, because
	// "how much of the day's flow cleared against an unvouched balance" is the
	// number that belongs on a dashboard. noteUngoverned makes the same split.
	g, seen := gateOverBook(t, concentrationMandate(90),
		bookWithCompleteness(&CashCompleteness{OmittedEntryTypes: []string{"corporate_action"}}))

	for i := 0; i < 3; i++ {
		if _, err := g.Evaluate(context.Background(), passingOrder()); err != nil {
			t.Fatal(err)
		}
	}
	if len(*seen) != 3 {
		t.Fatalf("observer fired %d times for 3 admissions, want 3 — a counter that only fires once "+
			"per portfolio cannot answer how much flow cleared this way", len(*seen))
	}
}
