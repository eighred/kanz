package bridge

import (
	"time"

	"context"
	"errors"
	"strings"
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
)

// testNow is the fixed instant the bridge tests reason about, and testFreshness
// is a bound wide enough that the freshness gate is never what a mandate test is
// measuring. The freshness gate has its own tests in freshness_test.go.
func testNow() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }

func testFreshness() Freshness {
	return Freshness{MaxAge: time.Hour, Now: testNow}
}

func sampleProposal() optimization.RebalanceProposal {
	return optimization.RebalanceProposal{
		PortfolioID: "PF",
		// A CHECKED-AND-CLEAN PROPOSAL, which only optimization.CheckMandate can
		// produce. It is spelled out here so the non-vacuity is visible: the tests
		// below assert that unchecked and infeasible proposals emit nothing, and
		// they would all pass against a ToOrders that emitted nothing ever.
		MandateStatus: optimization.MandateFeasible,
		// A DATED PROPOSAL, for the same non-vacuity reason as the verdict above
		// (#970): ToOrders now refuses an undated one, so a fixture without AsOf
		// would make every test below pass against a ToOrders that emitted nothing.
		AsOf: testNow(),
		Trades: []optimization.ProposedTrade{
			{InstrumentID: "AAA", Side: optimization.Buy, TargetWeight: 0.6, Quantity: 100},
			{InstrumentID: "BBB", Side: optimization.Sell, TargetWeight: 0.1, Quantity: 50},
		},
	}
}

func TestToOrders_MapsTradesIssuerBound(t *testing.T) {
	cmds, err := ToOrders(sampleProposal(), "alice@desk", testFreshness())
	if err != nil {
		t.Fatalf("a mandate-feasible proposal must materialize: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("want 2 orders, got %d", len(cmds))
	}
	byInstr := map[string]*orderpb.SubmitOrder{}
	for _, c := range cmds {
		byInstr[c.GetInstrumentId()] = c
		if c.GetMetadata().GetIssuer() != "alice@desk" {
			t.Fatalf("issuer not bound on %s", c.GetInstrumentId())
		}
		if c.GetOrderType() != orderpb.OrderType_ORDER_TYPE_MARKET {
			t.Fatalf("expected market order for %s", c.GetInstrumentId())
		}
		if c.GetPortfolioId() != "PF" {
			t.Fatalf("portfolio not propagated")
		}
	}
	if byInstr["AAA"].GetSide() != orderpb.Side_SIDE_BUY {
		t.Fatal("AAA should be a BUY")
	}
	if byInstr["BBB"].GetSide() != orderpb.Side_SIDE_SELL {
		t.Fatal("BBB should be a SELL")
	}
	// Quantity round-trips at 1e-4 scale: 100 units.
	q := byInstr["AAA"].GetQuantity()
	if got := float64(q.GetCoefficient()) * pow(q.GetExponent()); got != 100 {
		t.Fatalf("AAA quantity: got %v want 100", got)
	}
}

func TestToOrders_InfeasibleProposalEmitsNothing(t *testing.T) {
	p := sampleProposal()
	p.MandateStatus = optimization.MandateInfeasible
	p.Violations = []string{"instrument concentration over 50%"}
	cmds, err := ToOrders(p, "alice", testFreshness())
	if len(cmds) != 0 {
		t.Fatalf("mandate-infeasible proposal must emit no orders, got %d", len(cmds))
	}
	if !errors.Is(err, ErrMandateInfeasible) {
		t.Fatalf("an infeasible proposal must be refused as such, got %v", err)
	}
	if !strings.Contains(err.Error(), "concentration") {
		t.Errorf("the refusal drops the violation that caused it: %v", err)
	}
}

// AN UNCHECKED PROPOSAL IS REFUSED, AND NOT AS AN INFEASIBLE ONE (#646).
//
// This is the line the whole issue turns on. The gate used to be
// `if !p.MandateFeasible` over a bool that RebalanceProposal's own constructor
// set to true, so a proposal nothing had evaluated and one that had passed every
// rule were the same value here — and this function is what turns a proposal
// into live capital commands.
func TestToOrders_UncheckedProposalIsRefusedByName(t *testing.T) {
	p := sampleProposal()
	p.MandateStatus = optimization.MandateUnchecked // the zero value: what Rebalance produces
	cmds, err := ToOrders(p, "alice", testFreshness())
	if len(cmds) != 0 {
		t.Fatalf("a proposal no mandate check has run on must emit no orders, got %d", len(cmds))
	}
	if !errors.Is(err, ErrMandateUnchecked) {
		t.Fatalf("an unchecked proposal must be refused as UNCHECKED, got %v", err)
	}
	if errors.Is(err, ErrMandateInfeasible) {
		t.Error("'nothing checked it' was reported as 'it breaches its mandate' — the two are " +
			"different operator problems and must not share an error")
	}
}

// The refusal must reach Materialize's caller as an error, not as a quiet empty
// result: an armed publisher that saw zero commands and no error would report a
// successful materialization of nothing.
func TestMaterialize_RefusesAnUncheckedProposalLoudly(t *testing.T) {
	pub := &recordingPublisher{}
	p := sampleProposal()
	p.MandateStatus = optimization.MandateUnchecked
	res, err := Materialize(context.Background(), p, "t1", "alice", "USD", nil, nil, pub, testFreshness())
	if !errors.Is(err, ErrMandateUnchecked) {
		t.Fatalf("Materialize must propagate the refusal, got %v", err)
	}
	if len(pub.published) != 0 {
		t.Fatalf("nothing may be published for an unchecked proposal, got %d", len(pub.published))
	}
	if len(res.Submitted) != 0 {
		t.Fatalf("nothing may be reported as submitted, got %d", len(res.Submitted))
	}
}

// recordingPublisher captures published commands.
type recordingPublisher struct{ published []*orderpb.SubmitOrder }

func (r *recordingPublisher) Publish(_ context.Context, cmd *orderpb.SubmitOrder) error {
	r.published = append(r.published, cmd)
	return nil
}

// rejectGate rejects orders in a named instrument.
type rejectGate struct{ rejectInstrument string }

func (g rejectGate) Evaluate(_ context.Context, d compliance.OrderDelta) (compliance.Decision, error) {
	if d.InstrumentID == g.rejectInstrument {
		return compliance.Decision{Allowed: false}, nil
	}
	return compliance.Decision{Allowed: true}, nil
}

func TestMaterialize_GateRejectsAreNotPublished(t *testing.T) {
	pub := &recordingPublisher{}
	res, err := Materialize(context.Background(), sampleProposal(), "t1", "alice", "USD",
		map[string]float64{"AAA": 10, "BBB": 20}, rejectGate{rejectInstrument: "BBB"}, pub, testFreshness())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Submitted) != 1 || res.Submitted[0].GetInstrumentId() != "AAA" {
		t.Fatalf("only AAA should be submitted, got %+v", res.Submitted)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Command.GetInstrumentId() != "BBB" {
		t.Fatalf("BBB should be gate-rejected, got %+v", res.Rejected)
	}
	if len(pub.published) != 1 {
		t.Fatalf("only the admitted order should be published, got %d", len(pub.published))
	}
}

// unpricedGate refuses every order as Unpriced — the shape gate.Evaluate
// returns for a nil/zero/negative price (COMP-M1).
type unpricedGate struct{}

func (unpricedGate) Evaluate(_ context.Context, _ compliance.OrderDelta) (compliance.Decision, error) {
	return compliance.Decision{Allowed: false, Unpriced: true}, nil
}

// ungovernedGate refuses every order as Ungoverned — no mandate exists.
type ungovernedGate struct{}

func (ungovernedGate) Evaluate(_ context.Context, _ compliance.OrderDelta) (compliance.Decision, error) {
	return compliance.Decision{Allowed: false, Ungoverned: true}, nil
}

// TestMaterialize_UnpricedInstrumentRejectsWithoutClaimingABreach: an
// instrument absent from the prices map leaves orderDelta's Price nil, the
// gate refuses it as Unpriced, and the rejection reason must say so — NOTHING
// BREACHED, and reporting "pre-trade compliance breach" here is actively
// misleading (COMP-M1 Task 2).
func TestMaterialize_UnpricedInstrumentRejectsWithoutClaimingABreach(t *testing.T) {
	pub := &recordingPublisher{}
	res, err := Materialize(context.Background(), sampleProposal(), "t1", "alice", "USD",
		map[string]float64{"AAA": 10}, // BBB is absent ⇒ nil price ⇒ Unpriced
		unpricedGate{}, pub, testFreshness())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) == 0 {
		t.Fatal("an unpriced order must be rejected")
	}
	reason := res.Rejected[0].Reason
	if reason == "pre-trade compliance breach" {
		t.Fatalf("an Unpriced decision must not be reported as a breach — nothing was evaluated, got %q", reason)
	}
}

// TestMaterialize_UngovernedInstrumentRejectsWithoutClaimingABreach: the
// same pre-existing defect existed for Ungoverned — fixed alongside Unpriced
// since it is the identical principle in the same function.
func TestMaterialize_UngovernedInstrumentRejectsWithoutClaimingABreach(t *testing.T) {
	pub := &recordingPublisher{}
	res, err := Materialize(context.Background(), sampleProposal(), "t1", "alice", "USD",
		map[string]float64{"AAA": 10, "BBB": 20}, ungovernedGate{}, pub, testFreshness())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) == 0 {
		t.Fatal("an ungoverned order must be rejected")
	}
	reason := res.Rejected[0].Reason
	if reason == "pre-trade compliance breach" {
		t.Fatalf("an Ungoverned decision must not be reported as a breach — nothing governs the portfolio, got %q", reason)
	}
}

func TestMaterialize_PublishErrorPropagates(t *testing.T) {
	_, err := Materialize(context.Background(), sampleProposal(), "t1", "alice", "USD", nil, nil, errPublisher{}, testFreshness())
	if err == nil {
		t.Fatal("a publish error must propagate")
	}
}

type errPublisher struct{}

func (errPublisher) Publish(context.Context, *orderpb.SubmitOrder) error {
	return errors.New("bus down")
}

func pow(exp int32) float64 {
	p := 1.0
	for i := int32(0); i < exp; i++ {
		p *= 10
	}
	for i := int32(0); i < -exp; i++ {
		p /= 10
	}
	return p
}

// EVERY REFUSAL THE GATE CAN RETURN GETS ITS OWN REASON (#803).
//
// gateReason is the second consumer of a compliance.Decision that enumerated the
// "nothing was evaluated" family by hand, and the second one that was missing
// Unreadable — so a rebalance proposal rejected because the portfolio's mandate
// could not be decoded came back as "pre-trade compliance breach": a rule breach
// that never fired.
//
// The fall-through is the failure mode, so the assertion is that NO member of the
// family reaches it. A table over the flags rather than one test per flag,
// because the next flag added is the one this is written for.
func TestGateReason_NoRefusalFallsThroughToRuleBreach(t *testing.T) {
	const fallthroughReason = "pre-trade compliance breach"
	for _, tc := range []struct {
		name string
		d    compliance.Decision
		want string
	}{
		{"ungoverned", compliance.Decision{Ungoverned: true}, "no mandate governs"},
		{"unpriced", compliance.Decision{Unpriced: true}, "no usable price"},
		{"unvaluable", compliance.Decision{Unvaluable: true}, "notional cannot be represented"},
		{"unscoped", compliance.Decision{Unscoped: true}, "which tenant's mandate"},
		{"unreadable", compliance.Decision{Unreadable: true}, "must be republished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gateReason(tc.d)
			if got == fallthroughReason {
				t.Fatalf("a %s refusal renders as %q — nothing was evaluated, so calling it a "+
					"compliance breach sends a reviewer to look for the rule that fired",
					tc.name, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason %q does not contain %q", got, tc.want)
			}
		})
	}
}

// NON-VACUITY: a real rule violation must still render as its own message, and a
// decision with no flags and no result still falls through — that tail is the
// correct answer for an actual breach with nothing attached.
func TestGateReason_ARealViolationStillRendersItsOwnMessage(t *testing.T) {
	d := compliance.Decision{Result: &compliancepb.ComplianceResult{
		Violations: []*compliancepb.Violation{{Message: "concentration limit exceeded"}},
	}}
	if got := gateReason(d); got != "concentration limit exceeded" {
		t.Fatalf("gateReason = %q, want the violation's own message", got)
	}
}
