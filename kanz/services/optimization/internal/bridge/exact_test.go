package bridge

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/optimization"
	"google.golang.org/protobuf/proto"
)

func exactFixture() (optimization.ExactRebalanceProposal, ExactExecutionBounds, Freshness) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	p := optimization.ExactRebalanceProposal{PortfolioID: "fund", AsOf: now, MandateStatus: optimization.MandateFeasible,
		Trades: []optimization.ExactTrade{{InstrumentID: "A", Side: optimization.Buy, Quantity: "0.000000001", Notional: "0.000000001"}}}
	b := ExactExecutionBounds{MaxNotional: "10", MaxBuyNotional: "10", ExpiresAt: now.Add(time.Minute),
		Lots: map[string]dec.Exact{"A": "0.000000001"}, Ticks: map[string]dec.Exact{"A": "0.01"}}
	return p, b, Freshness{MaxAge: time.Minute, Now: func() time.Time { return now }}
}

func TestExactOrdersPreserveQuantityAndRevisionIdentity(t *testing.T) {
	p, b, f := exactFixture()
	first, err := ExactOrders(p, "tenant", "revision-1", "maker", b, f)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Quantity.Coefficient != 1 || first[0].Quantity.Exponent != -9 || first[0].ParentOrderId != "" {
		t.Fatalf("financial terms or ordinary OMS admission changed: %v", first[0])
	}
	again, err := ExactOrders(p, "tenant", "revision-1", "maker", b, f)
	if err != nil || !proto.Equal(first[0], again[0]) {
		t.Fatalf("retry changed command: %v", err)
	}
	for _, identity := range [][2]string{{"tenant", "revision-2"}, {"tenant-two", "revision-1"}} {
		next, err := ExactOrders(p, identity[0], identity[1], "maker", b, f)
		if err != nil || next[0].OrderId == first[0].OrderId {
			t.Fatalf("identity collision: %v", err)
		}
	}
	p.Trades[0].Quantity, p.Trades[0].Notional = "9007199254740993", "9007199254740993"
	b.MaxNotional, b.MaxBuyNotional, b.Lots["A"] = "9007199254740993", "9007199254740993", "1"
	large, err := ExactOrders(p, "tenant", "revision-3", "maker", b, f)
	if err != nil || large[0].Quantity.Coefficient != 9007199254740993 {
		t.Fatalf("large quantity changed: %v", err)
	}
}

func TestExactOrdersRefuseEntirePlan(t *testing.T) {
	tests := map[string]func(*optimization.ExactRebalanceProposal, *ExactExecutionBounds, *Freshness){
		"unchecked": func(p *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, _ *Freshness) {
			p.MandateStatus = optimization.MandateUnchecked
		},
		"missing lots":   func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) { b.Lots = nil },
		"missing ticks":  func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) { b.Ticks = nil },
		"fractional lot": func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) { b.Lots["A"] = "1" },
		"unknown cash": func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			b.MaxBuyNotional = ""
		},
		"insufficient cash": func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			b.MaxBuyNotional = "0"
		},
		"expiry": func(p *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			b.ExpiresAt = p.AsOf
		},
		"future": func(p *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, _ *Freshness) {
			p.AsOf = p.AsOf.Add(time.Hour)
		},
		"stale": func(p *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, _ *Freshness) {
			p.AsOf = p.AsOf.Add(-time.Hour)
		},
		"unbounded freshness": func(_ *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, f *Freshness) { f.MaxAge = 0 },
		"unbounded notional": func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			b.MaxNotional = "0"
		},
		"unknown side": func(p *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, _ *Freshness) {
			p.Trades[0].Side = 99
		},
		"duplicate instrument": func(p *optimization.ExactRebalanceProposal, _ *ExactExecutionBounds, _ *Freshness) {
			p.Trades = append(p.Trades, p.Trades[0])
		},
		"repeating quantity": func(p *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			p.Trades[0].Quantity = "1/3"
			b.Lots["A"] = "1/3"
		},
		"unrepresentable quantity": func(p *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			p.Trades[0].Quantity = "9223372036854775809"
			b.Lots["A"] = "1"
		},
		"unrepresentable price": func(p *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			p.Trades[0].Quantity = "1"
			p.Trades[0].Notional = "1/3"
			b.Ticks["A"] = "1/3"
		},
		"invalid slippage": func(_ *optimization.ExactRebalanceProposal, b *ExactExecutionBounds, _ *Freshness) {
			b.SlippageBPS = 10000
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p, b, f := exactFixture()
			mutate(&p, &b, &f)
			cmds, err := ExactOrders(p, "tenant", "revision", "maker", b, f)
			if err == nil || len(cmds) != 0 {
				t.Fatalf("invalid plan admitted: %v %v", cmds, err)
			}
		})
	}
}

func TestExactOrdersTightenTicksAndBoundAggregateCash(t *testing.T) {
	p, b, f := exactFixture()
	p.Trades = []optimization.ExactTrade{
		{InstrumentID: "B", Side: optimization.Sell, Quantity: "1", Notional: "1.005"},
		{InstrumentID: "A", Side: optimization.Buy, Quantity: "1", Notional: "1.005"},
	}
	b.Lots = map[string]dec.Exact{"A": "1", "B": "1"}
	b.Ticks = map[string]dec.Exact{"A": "0.01", "B": "0.01"}
	cmds, err := ExactOrders(p, "tenant", "revision", "maker", b, f)
	if err != nil {
		t.Fatal(err)
	}
	if cmds[0].InstrumentId != "A" || cmds[0].LimitPrice.Coefficient != 1 || cmds[0].LimitPrice.Exponent != 0 || cmds[1].LimitPrice.Coefficient != 101 || cmds[1].LimitPrice.Exponent != -2 {
		t.Fatalf("tick alignment loosened a limit: %v", cmds)
	}
	p.Trades[0].Side = optimization.Buy
	b.MaxBuyNotional = "1.5"
	cmds, err = ExactOrders(p, "tenant", "revision", "maker", b, f)
	if err == nil || len(cmds) != 0 {
		t.Fatal("split buys escaped aggregate cash ceiling")
	}
	b.MaxBuyNotional, b.MaxNotional = "10", "2"
	cmds, err = ExactOrders(p, "tenant", "revision", "maker", b, f)
	if err == nil || len(cmds) != 0 {
		t.Fatal("split trades escaped aggregate notional ceiling")
	}
}

func TestExactOrdersSlippageCannotExceedCapitalCeiling(t *testing.T) {
	p, b, f := exactFixture()
	p.Trades[0].Quantity, p.Trades[0].Notional = "1", "100"
	b.Lots["A"] = "1"
	b.MaxNotional, b.MaxBuyNotional, b.SlippageBPS = "100", "101", 100
	cmds, err := ExactOrders(p, "tenant", "revision", "maker", b, f)
	if err == nil || len(cmds) != 0 {
		t.Fatal("reference-price ceiling allowed a more expensive executable buy")
	}
}
