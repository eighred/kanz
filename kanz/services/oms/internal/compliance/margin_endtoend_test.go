package compliance

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/venuemargin"
)

// #408 CONTROL 3, END TO END: a SubmitOrder, the shipped rule engine, the
// shipped account bindings, and the shipped venuemargin fold.
//
// The unit tests either side of this prove the rule's arithmetic and the
// adapter's join. What they cannot prove is that the two are actually CONNECTED
// on the order-admission path — that OrderDelta carries the venue, that the gate
// binds the margin closure, and that the refusal reaches the OMS as a terminal
// breach rather than an error that retries forever. That wiring is the part
// composition roots have shipped broken twice on this estate with a green suite.

func marginMandate(venue string) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m-margin", TenantId: "acme", PortfolioId: "fund-alpha", Version: 1,
		EffectiveAt: timestamppb.New(time.Unix(0, 0)),
		Rules: []*compliancepb.Rule{{
			RuleId: "vm-1", Type: compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN,
			Params: &compliancepb.Rule_VenueMargin{
				VenueMargin: &compliancepb.VenueMarginLimit{Venue: venue},
			},
		}},
	}
}

func marginOrder() *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		Metadata:     &commandpb.CommandMetadata{Issuer: "trader@desk"},
		OrderId:      "o-1",
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 1},
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   &commonpb.Decimal{Coefficient: 60000},
		Venue:        testVenue,
	}
}

// gateWith builds the shipped pre-trade gate over a margin source reading the
// given view, with a mandate declaring venue margin at testVenue.
func gateWith(t *testing.T, view MarginView) *COMP01Gate {
	t.Helper()
	reg := comp.NewMandateRegistry()
	if err := reg.Put(marginMandate(testVenue)); err != nil {
		t.Fatalf("registry refused the mandate: %v", err)
	}
	pre := comp.NewPreTradeGate(nil, comp.MapBookSource{}, reg, nil, nil, nil,
		comp.WithMarginSource(NewMarginSource(bindings(t, testSpec), view)))
	return NewCOMP01Gate(pre, "USD")
}

// A BUY ON A MARGIN-ENABLED PORTFOLIO IS REFUSED WHEN NOTHING HAS OBSERVED THE
// ACCOUNT — as a TERMINAL breach carrying the rule's own code.
//
// Terminal matters: an error would be redelivered, and the margin state will not
// appear because the order came back. The trader would wait while the command
// cycled.
func TestCheck_UnknownMarginRefusesTheOrder(t *testing.T) {
	g := gateWith(t, venuemargin.New())

	breach, err := g.Check(context.Background(), "acme", marginOrder())
	if err != nil {
		t.Fatalf("unknown margin must be a terminal Breach, not an error: %v", err)
	}
	if breach == nil {
		t.Fatal("an order that would TAKE a position was ADMITTED against an account whose margin " +
			"state nothing has ever observed — #408 control 3 is not connected to the admission path")
	}
	if breach.Code != "VENUE_MARGIN" {
		t.Fatalf("want Code=VENUE_MARGIN so a reviewer knows which control fired, got %+v", breach)
	}
}

// THE SAME ORDER IS ADMITTED ONCE THE VENUE HAS ANSWERED, current and complete.
//
// Without this the test above is satisfied by a gate that refuses everything,
// which is a trading outage rather than a control.
func TestCheck_ObservedMarginAdmitsTheOrder(t *testing.T) {
	now := time.Now().UTC()
	view := venuemargin.New()
	observe(t, view, state(now, &commonpb.Decimal{Coefficient: 5, Exponent: -1}, fullCoverage()))

	breach, err := gateWith(t, view).Check(context.Background(), "acme", marginOrder())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("a current, fully covered margin state still refused the order: %+v", breach)
	}
}

// AN ORDER ROUTED SOMEWHERE THE MANDATE DOES NOT GOVERN IS NOT REFUSED BY THIS
// RULE. It is not the rule's venue, so the rule has nothing to say.
func TestCheck_OrderToAnotherVenueIsNotGatedByThisRule(t *testing.T) {
	cmd := marginOrder()
	cmd.Venue = "XBIN"

	breach, err := gateWith(t, venuemargin.New()).Check(context.Background(), "acme", cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("an OKX margin rule refused an order routed to Binance: %+v", breach)
	}
}
