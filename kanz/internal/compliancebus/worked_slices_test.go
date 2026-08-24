package compliancebus

import (
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	comp "github.com/eighred/kanz/internal/compliance"
)

// THE AUDIT LOG MUST BE READABLE BACKWARDS FROM A CHILD ORDER (#435, #484).
//
// A scheduled parent is checked ONCE, for the whole notional, and its children
// are admitted without re-checking — deliberately, because every limit is
// evaluated against a book that only moves on fills, so N children inside one
// window each see the same unchanged book and the sum is never tested (#483).
//
// The consequence is that the fills land on N order ids and only the PARENT has
// a decision record. A regulator asking "why was this trade allowed" holds a
// child's id; parent_order_id gets them to the parent, but nothing on the
// decision said it authorised more than the one order it names.

func result() *compliancepb.ComplianceResult {
	return &compliancepb.ComplianceResult{
		PortfolioId: "fund-alpha", MandateId: "m-1",
		Status: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
	}
}

func TestDecisionLog_AScheduledOrdersDecisionSaysHowManyOrdersItAuthorises(t *testing.T) {
	log := BuildDecisionLog(comp.DecisionRecord{
		Phase: comp.PhasePreTrade, Result: result(), Allowed: true,
		OrderID: "3f9a1c2e0b7d4e119a6f2c8d5e4b7a13", WorkedSlices: 6,
	})
	if got := log.GetAttributes()["worked_slices"]; got != "6" {
		t.Fatalf("worked_slices = %q, want 6 — this decision authorised six child orders and the "+
			"record names only one of them, so an auditor arriving from a child cannot tell "+
			"whether they have found the whole authorisation or a fragment of it", got)
	}
	if got := log.GetAttributes()["order_id"]; got != "3f9a1c2e0b7d4e119a6f2c8d5e4b7a13" {
		t.Errorf("order_id = %q, want the PARENT — it is the order the decision was made about", got)
	}
}

// AN ORDINARY ORDER SAYS NOTHING, rather than saying "1".
//
// A decision that authorises exactly the order it names is the ordinary case,
// and the absence of the attribute IS that statement. Emitting worked_slices=1
// for every order would make the interesting case indistinguishable from the
// common one at a glance, which is the whole reason the attribute exists.
func TestDecisionLog_AnOrdinaryOrderCarriesNoSliceCount(t *testing.T) {
	log := BuildDecisionLog(comp.DecisionRecord{
		Phase: comp.PhasePreTrade, Result: result(), Allowed: true, OrderID: "o1",
	})
	if got, ok := log.GetAttributes()["worked_slices"]; ok {
		t.Errorf("worked_slices = %q on an order nobody sliced; absence is the statement", got)
	}
}
