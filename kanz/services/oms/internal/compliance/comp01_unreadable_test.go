package compliance

import (
	"context"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

// AN UNREADABLE MANDATE IS NOT A RULE BREACH (#803).
//
// # What was wrong, and what was not
//
// Check branches on Ungoverned, Unpriced, Unvaluable and Unscoped, then falls
// through to breachFromResult(decision.Result). For Decision{Unreadable: true}
// the Result is NIL, so breachFromResult hit its defensive tail and returned
// Code "MANDATE", Reason "mandate breach".
//
// Fail-closed held — the order IS refused. What was wrong is the ATTRIBUTION.
// The client and the audit trail were told COMPLIANCE_MANDATE / "mandate breach"
// for an order against which NO RULE WAS EVALUATED. That is exactly the collapse
// the four sibling branches exist to prevent, in their own words: "A reviewer
// reading MANDATE_MISSING knows to go and write a mandate — not to go and look
// for the rule that fired."
//
// # Why the mis-signposting is the whole cost
//
// The mandate stream is COMPACTED. The message that failed to decode is the last
// one on that portfolio's subject, so every consumer that boots re-reads it and
// fails identically — which is why the gate makes it terminal rather than
// retrying. So this refuses EVERY order for that portfolio, indefinitely, and the
// desk reading "mandate breach" reasonably concludes the mandate is working. The
// real fix — republish the mandate — was named nowhere in the rejection.

// unreadableMandates is a mandate source whose published mandate cannot be
// applied: the shape MandateRegistry takes on after a poison ConfigChanged.
type unreadableMandates struct{}

func (unreadableMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	return nil, comp.GovernanceUnspecified, comp.ErrMandateUnreadable
}

func unreadableGate(t *testing.T) *COMP01Gate {
	t.Helper()
	gate := comp.NewPreTradeGate(comp.NewEngine(nil), comp.MapBookSource{}, unreadableMandates{}, nil, nil, nil)
	return NewCOMP01Gate(gate, "USD")
}

func TestCheck_AnUnreadableMandateMapsToItsOwnBreachCode(t *testing.T) {
	g := unreadableGate(t)

	breach, err := g.Check(context.Background(), bus.SystemTenant, pricedOrder())
	// A TERMINAL CONDITION IS A VERDICT, NOT A REDELIVERY. Returning an error here
	// would nack a command whose next delivery re-reads the same undecodable
	// bytes, forever.
	if err != nil {
		t.Fatalf("an unreadable mandate must be a verdict, not a retryable error: %v", err)
	}
	if breach == nil {
		t.Fatal("the order was ADMITTED although the mandate governing it could not be read, so " +
			"no rule was evaluated against it")
	}
	if breach.Code != "MANDATE_UNREADABLE" {
		t.Fatalf("code %q, want MANDATE_UNREADABLE — %q sends a reviewer to look for the rule "+
			"that fired, and no rule ran at all. The mandate stream is compacted, so this "+
			"portfolio refuses every order until somebody republishes, and nothing in the "+
			"rejection says so", breach.Code, breach.Code)
	}
	// THE REASON MUST NAME THE OPERATOR ACTION. This string reaches the submitting
	// client on the ORDER_REJECTED FACT, and "mandate breach" sent them looking
	// for a limit nobody exceeded.
	if !strings.Contains(breach.Reason, "republish") {
		t.Errorf("reason %q does not name the action that resolves it — the mandate must be "+
			"republished, and no retry or rule review will do it", breach.Reason)
	}
}

// THE CODE THE CLIENT SEES IS THE CODE THE AUDIT TRAIL FILES IT UNDER.
//
// internal/compliance's notEvaluatedCode already mapped Unreadable to
// MANDATE_UNREADABLE for the decision record. Check re-implemented the same table
// as a chain of string literals and was missing this row — so for a while the two
// halves of one refusal disagreed about what it was. They are now read from one
// place; this asserts they still agree.
func TestCheck_TheClientCodeMatchesTheRecordedCode(t *testing.T) {
	g := unreadableGate(t)
	breach, err := g.Check(context.Background(), bus.SystemTenant, pricedOrder())
	if err != nil || breach == nil {
		t.Fatalf("precondition: breach=%v err=%v", breach, err)
	}
	if want := comp.CodeMandateUnreadable; breach.Code != want {
		t.Fatalf("client code %q != recorded code %q — one refusal, two names, and an operator "+
			"correlating the rejection with the audit record finds neither",
			breach.Code, want)
	}
}

// NON-VACUITY. The four sibling refusals must keep their own codes: a fix that
// made everything MANDATE_UNREADABLE would pass the test above and destroy the
// distinction the whole family exists for.
func TestCheck_TheSiblingRefusalsKeepTheirOwnCodes(t *testing.T) {
	reg := comp.NewMandateRegistry()
	gate := comp.NewPreTradeGate(comp.NewEngine(nil), comp.MapBookSource{}, reg, nil, nil, nil)
	g := NewCOMP01Gate(gate, "USD")

	// No mandate published at all: MANDATE_MISSING, not MANDATE_UNREADABLE.
	// requireMandate is off, so an ungoverned portfolio is admitted — which is
	// itself the distinction: "nobody wrote one" is a posture, "one exists and
	// cannot be read" is not.
	breach, err := g.Check(context.Background(), bus.SystemTenant, pricedOrder())
	if err != nil {
		t.Fatalf("ungoverned lookup: %v", err)
	}
	if breach != nil && breach.Code == "MANDATE_UNREADABLE" {
		t.Fatal("a portfolio with NO mandate was reported as having an unreadable one — the two " +
			"send a reviewer to opposite places")
	}

	// An unpriced order keeps PRICE_UNAVAILABLE. It needs a gate with a mandate
	// and a book — an ungoverned portfolio short-circuits before price is ever
	// looked at, which is the ordering the family depends on.
	priced := NewCOMP01Gate(bookedTestGate(t), "USD")
	unpricedBreach, err := priced.Check(context.Background(), "t1",
		unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("unpriced check: %v", err)
	}
	if unpricedBreach == nil || unpricedBreach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("an unpriced order returned %v, want PRICE_UNAVAILABLE — the refusal family "+
			"must keep its members apart", unpricedBreach)
	}
}
