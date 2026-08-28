package main

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/config"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
)

// THE COMPOSITION ROOT IS WHERE THIS DEFECT WOULD RETURN (#787).
//
// The monitor's own tests prove that a book with cash and marks behind it can be
// valued. What they cannot reach is whether a real startup WIRES either — and
// that is the failure this repository keeps paying for: services/oms once passed
// a bare nil for its decision recorder and the enforcement point that decides
// whether capital moves recorded nothing anywhere, with a green suite (#643).
//
// While these were locals in runConsumers there was nothing to call. Two
// properties in particular were unassertable: whether the mark fold got the
// CONFIGURED staleness bound rather than a default, and whether a cash
// announcement actually re-evaluates the portfolio it names rather than merely
// updating a number.

type fakeBus struct{ events []bus.Event }

// Publish enforces the caller-facing preconditions the real producer enforces.
// The tenant rule is the one that matters: Emitter.EmitBreach sets no
// Event.TenantID and this service configures no producer fallback, so an empty
// tenant is a hard publish failure in production. A double that accepted it
// would certify nothing — which is how the sweep's missing tenant stamp reached
// a passing suite in the first place.
func (f *fakeBus) Publish(ctx context.Context, e bus.Event) error {
	if e.TenantID == "" && bus.TenantIDFromContext(ctx) == "" {
		return errTenantRequired
	}
	f.events = append(f.events, e)
	return nil
}

var errTenantRequired = errStr("envelope validation: tenant_id required")

type errStr string

func (e errStr) Error() string { return string(e) }

func dec(c int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: c, Exponent: exp}
}

func testConfig(t *testing.T, maxAge time.Duration) config.Config {
	t.Helper()
	return config.Config{
		PriceSubjects:      []string{"market.*.trade"},
		PriceMaxAge:        maxAge,
		ReevaluateInterval: time.Minute,
	}
}

func TestTheBuiltValuationCarriesBothFolds(t *testing.T) {
	v := buildPostTradeValuation(testConfig(t, time.Hour), prometheus.NewRegistry(), nil)

	if v.Cash == nil {
		t.Error("no cash view was built. Without it every book's equity is a positions total and " +
			"every gross-leverage rule refuses (#787)")
	}
	if v.Marks == nil {
		t.Error("no mark fold was built. Without it the book is valued at whatever the position " +
			"projector recorded, which is average COST")
	}
}

// TestTheMarkFoldHonoursTheConfiguredStalenessBound. COMPLIANCE_PRICE_MAX_AGE is
// a safety bound, not a tuning knob: a fold built with the package default
// instead of the configured value would keep valuing a book off a feed that
// stopped reporting, and the leverage cap would be enforced against a number
// that no longer moves. That the CONFIGURED value reaches the fold is only
// observable here.
func TestTheMarkFoldHonoursTheConfiguredStalenessBound(t *testing.T) {
	v := buildPostTradeValuation(testConfig(t, time.Minute), prometheus.NewRegistry(), nil)

	// A print an hour old, against a one-minute bound.
	foldPrint(t, v, "AAPL", 100, time.Now().UTC().Add(-time.Hour))
	if _, _, seen := v.Marks.Lookup("AAPL"); !seen {
		t.Fatal("the fold rejected the print outright — this fixture is not testing staleness")
	}
	if v.Marks.Mark("AAPL") != nil {
		t.Fatal("an hour-old price is live under a one-minute bound: the configured " +
			"COMPLIANCE_PRICE_MAX_AGE did not reach the fold")
	}

	// And a current print IS usable, so the assertion above is not satisfied by a
	// fold that refuses everything.
	foldPrint(t, v, "MSFT", 200, time.Now().UTC())
	if v.Marks.Mark("MSFT") == nil {
		t.Fatal("a current price is not usable — the bound is refusing everything")
	}
}

// TestACashAnnouncementReevaluatesThePortfolio is the wiring property. Cash is
// half of equity, so drawing on a margin loan raises leverage with NO position
// FACT behind it; a handler that only folded the number would leave that breach
// waiting for an unrelated event.
func TestACashAnnouncementReevaluatesThePortfolio(t *testing.T) {
	fb := &fakeBus{}
	v := buildPostTradeValuation(testConfig(t, time.Hour), prometheus.NewRegistry(), nil)
	mon := monitor.NewMonitor(comp.NewEngine(nil), leverageRegistry(t), nil, monitor.NewEmitter(fb), nil, nil,
		monitor.WithCashSource(v.Cash), monitor.WithMarkSource(v.Marks))

	now := time.Now().UTC()
	foldPrint(t, v, "AAPL", 100, now)
	// $100,000 of stock against $50,000 of equity: 2.0x, inside the 2.5x cap.
	announce(t, v, mon, "p1", -50_000, now)
	holdPosition(t, mon, 1_000, 100_000, now)
	if len(fb.events) != 0 {
		t.Fatalf("a book at 2.0x breached a 2.5x cap on the way in: %d event(s)", len(fb.events))
	}

	// THE FUND DRAWS FURTHER on the same holdings: equity $30,000, 3.33x. Nothing
	// else happens — no order, no fill, no position FACT.
	announce(t, v, mon, "p1", -70_000, now)

	if len(fb.events) == 0 {
		t.Fatal("a cash announcement took the portfolio from 2.0x to 3.33x under a 2.5x cap and " +
			"nothing was emitted. CashHandler folded the balance and did not re-evaluate, which " +
			"leaves the breach waiting for an unrelated event (#787)")
	}
}

// --- fixtures ---------------------------------------------------------------

func foldPrint(t *testing.T, v postTradeValuation, instrument string, price int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: dec(price, 0)}},
	})
	if err != nil {
		t.Fatalf("marshal print: %v", err)
	}
	if err := v.Marks.Handle(context.Background(), &envelopepb.Envelope{EventType: "market.instrument.trade"}, payload); err != nil {
		t.Fatalf("fold print: %v", err)
	}
}

// announce drives the REAL handler the composition root subscribes — fold and
// re-evaluate together, which is the thing under test.
func announce(t *testing.T, v postTradeValuation, mon *monitor.Monitor, portfolio string, total int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:   portfolio,
		BaseCurrency:  "USD",
		Total:         dec(total, 0),
		AsOf:          timestamppb.New(at),
		KnowledgeTime: timestamppb.New(at),
	})
	if err != nil {
		t.Fatalf("marshal balance: %v", err)
	}
	ctx := bus.WithTenantID(context.Background(), "t1")
	if err := v.CashHandler(mon)(ctx, &envelopepb.Envelope{TenantId: "t1"}, payload); err != nil {
		t.Fatalf("cash handler: %v", err)
	}
}

func holdPosition(t *testing.T, mon *monitor.Monitor, qty, cost int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&domainpb.PositionState{
		PortfolioId:  "p1",
		InstrumentId: "AAPL",
		Quantity:     dec(qty, 0),
		MarketValue:  &commonpb.Money{Amount: dec(cost, 0), CurrencyCode: "USD"},
		AsOf:         timestamppb.New(at),
	})
	if err != nil {
		t.Fatalf("marshal position: %v", err)
	}
	ctx := bus.WithTenantID(context.Background(), "t1")
	if err := mon.Handle(ctx, &envelopepb.Envelope{TenantId: "t1"}, payload); err != nil {
		t.Fatalf("position handler: %v", err)
	}
}

func leverageRegistry(t *testing.T) *comp.MandateRegistry {
	t.Helper()
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		Rules: []*compliancepb.Rule{{
			RuleId: "lev-1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				MaxGrossLeverage: dec(25, -1), // 2.5x
			}},
		}},
	}); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}
	return reg
}
