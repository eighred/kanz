package compliance

import (
	"context"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/oms/internal/position"
)

// THE WHOLE CHAIN, #450 END TO END.
//
// accounting announces what a portfolio can spend → the OMS folds it into a
// cashview → BookSource merges it onto the compliance Book → the pre-trade gate
// refuses an order the portfolio cannot pay for.
//
// Each link is pinned in its own package. This is the one that fails if any two
// of them stop agreeing — which is the failure that would otherwise show up only
// as orders being refused in production for a reason nobody could attribute.

type stubStore struct{ snap *domainpb.PortfolioSnapshot }

func (s stubStore) Snapshot(context.Context, string, time.Time) (*domainpb.PortfolioSnapshot, error) {
	return s.snap, nil
}

// Apply is never called on the read path this test drives; position.Store
// requires it, and a panic here would say so loudly if that ever changed.
func (s stubStore) Apply(context.Context, string, *orderpb.Fill, time.Time, position.Announcer) (*position.Applied, error) {
	panic("the pre-trade read path must not fold a fill")
}

// Outbox is never reached either: this stub stands in for the READ side of
// position.Store, and the queue exists for the announce side.
func (s stubStore) Outbox() outbox.Queue {
	panic("the pre-trade read path must not announce a position")
}

func snapshotFor(portfolioID string) *domainpb.PortfolioSnapshot {
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolioID,
			BaseCurrency:     "USD",
			TotalMarketValue: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1_000_000}, CurrencyCode: "USD"},
			// CashBalance deliberately UNSET — the position book does not know it,
			// which is the whole reason accounting announces it.
		},
	}
}

func announce(t *testing.T, v *cashview.View, portfolioID string, total int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:   portfolioID,
		BaseCurrency:  "USD",
		Total:         &commonpb.Decimal{Coefficient: total},
		AsOf:          timestamppb.New(at),
		KnowledgeTime: timestamppb.New(at),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("fold announcement: %v", err)
	}
}

func buyingPowerMandate(portfolioID string) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: portfolioID,
		Rules: []*compliancepb.Rule{{
			RuleId: "bp-1",
			Type:   compliancepb.RuleType_RULE_TYPE_BUYING_POWER,
			Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{}},
		}},
	}
}

type stubMandate struct{ m *compliancepb.Mandate }

func (s stubMandate) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, bool, error) {
	return s.m, true, nil
}

func gateOver(t *testing.T, v *cashview.View) *comp.PreTradeGate {
	t.Helper()
	src := NewBookSource(stubStore{snapshotFor("PF1")}, v, nil, nil)
	return comp.NewPreTradeGate(comp.NewEngine(nil), src, stubMandate{buyingPowerMandate("PF1")}, nil, nil, nil)
}

func buyOrder(qty int64) comp.OrderDelta {
	return comp.OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: qty},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD", OrderID: "o1",
	}
}

func TestAnnouncedCashReachesTheGate(t *testing.T) {
	at := time.Now().UTC()
	v := cashview.New()
	announce(t, v, "PF1", 250, at)
	g := gateOver(t, v)

	// 3 x 100 = 300, against 250 announced.
	res, err := g.Evaluate(context.Background(), buyOrder(3))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Allowed {
		t.Fatal("an order for 300 was admitted against an announced balance of 250 — the " +
			"announcement did not reach the gate")
	}

	// 2 x 100 = 200 is affordable.
	res, err = g.Evaluate(context.Background(), buyOrder(2))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("an affordable order was refused: %v — a gate that refuses everything is an "+
			"outage, not a control", res.Result.GetViolations())
	}
}

// WITH NO ANNOUNCEMENT THE GATE REFUSES, and that is the designed posture: the
// snapshot carries no cash, so the balance is UNKNOWN, and unknown affordability
// is not affordability.
func TestWithNoAnnouncementTheGateRefuses(t *testing.T) {
	g := gateOver(t, cashview.New())
	res, err := g.Evaluate(context.Background(), buyOrder(1))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Allowed {
		t.Fatal("a portfolio with no announced balance was admitted under a buying-power " +
			"mandate — unknown cash must fail closed")
	}
}

// A NIL CASH SOURCE IS THE SAME POSTURE, so a deployment that has not wired the
// cash spine refuses under a spending mandate rather than admitting on nothing.
func TestANilCashSourceFailsClosed(t *testing.T) {
	src := NewBookSource(stubStore{snapshotFor("PF1")}, nil, nil, nil)
	g := comp.NewPreTradeGate(comp.NewEngine(nil), src, stubMandate{buyingPowerMandate("PF1")}, nil, nil, nil)
	res, err := g.Evaluate(context.Background(), buyOrder(1))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Allowed {
		t.Fatal("an unwired cash spine admitted an order under a buying-power mandate")
	}
}
