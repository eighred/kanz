package bridge

import (
	"context"
	"errors"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
)

func sampleProposal() optimization.RebalanceProposal {
	return optimization.RebalanceProposal{
		PortfolioID:     "PF",
		MandateFeasible: true,
		Trades: []optimization.ProposedTrade{
			{InstrumentID: "AAA", Side: optimization.Buy, TargetWeight: 0.6, Quantity: 100},
			{InstrumentID: "BBB", Side: optimization.Sell, TargetWeight: 0.1, Quantity: 50},
		},
	}
}

func TestToOrders_MapsTradesIssuerBound(t *testing.T) {
	cmds := ToOrders(sampleProposal(), "alice@desk")
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
	p.MandateFeasible = false
	if cmds := ToOrders(p, "alice"); len(cmds) != 0 {
		t.Fatalf("mandate-infeasible proposal must emit no orders, got %d", len(cmds))
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
	res, err := Materialize(context.Background(), sampleProposal(), "alice", "USD",
		map[string]float64{"AAA": 10, "BBB": 20}, rejectGate{rejectInstrument: "BBB"}, pub)
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
	res, err := Materialize(context.Background(), sampleProposal(), "alice", "USD",
		map[string]float64{"AAA": 10}, // BBB is absent ⇒ nil price ⇒ Unpriced
		unpricedGate{}, pub)
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
	res, err := Materialize(context.Background(), sampleProposal(), "alice", "USD",
		map[string]float64{"AAA": 10, "BBB": 20}, ungovernedGate{}, pub)
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
	_, err := Materialize(context.Background(), sampleProposal(), "alice", "USD", nil, nil, errPublisher{})
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
