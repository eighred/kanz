package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/marketdata/mark"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Inputs are owned by this single-portfolio synthetic control plane. The
// client may submit an order. It cannot supply a book, mandate or market fix.
type syntheticControls struct {
	tenant, portfolio string
	book              *compliance.Book
	mandate           *compliancepb.Mandate
	quote             *marketpb.MarketDataEvent
	observed          time.Time
}

func newSyntheticControls(now time.Time) syntheticControls {
	const portfolio = "synthetic-portfolio"
	usd := &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1000}}
	return syntheticControls{
		tenant: "synthetic-tenant", portfolio: portfolio,
		book: &compliance.Book{PortfolioID: portfolio, BaseCurrency: "USD", Cash: usd, NAV: usd, NAVBasis: compliance.NAVBasisEquity, CashCompleteness: &compliance.CashCompleteness{}},
		mandate: &compliancepb.Mandate{TenantId: "synthetic-tenant", PortfolioId: portfolio, MandateId: "synthetic-spot", Version: 1, EffectiveAt: timestamppb.New(now.Add(-time.Hour)), Rules: []*compliancepb.Rule{
			{RuleId: "synthetic-gross-leverage", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE, Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{MaxGrossLeverage: &commonpb.Decimal{Coefficient: 1}}}},
			{RuleId: "synthetic-buying-power", Type: compliancepb.RuleType_RULE_TYPE_BUYING_POWER, Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{MinCashAfter: &commonpb.Decimal{Coefficient: 100}}}},
		}},
		quote:    &marketpb.MarketDataEvent{InstrumentId: "SYNTH-USD", EventTime: timestamppb.New(now.Add(-2 * time.Second)), Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: &commonpb.Decimal{Coefficient: 90}, AskPrice: &commonpb.Decimal{Coefficient: 91}}}},
		observed: now.Add(-time.Second),
	}
}

type syntheticEvidence struct {
	kind, sourceID               string
	effective, observed, expires time.Time
}

// Only the durable gateway frame is an input to the order path. A completely
// validated set is returned together so no partial approval survives refusal.
func (c syntheticControls) evaluate(ctx context.Context, frame *envelopepb.EventFrame, now time.Time) ([]syntheticEvidence, error) {
	env := frame.GetEnvelope()
	var cmd orderpb.SubmitOrder
	if err := proto.Unmarshal(frame.GetPayload(), &cmd); err != nil {
		return nil, err
	}
	meta := cmd.GetMetadata()
	if env.GetSource() != "api-gateway" || env.GetEventType() != "order.order.submit" || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND || env.GetEventId() == "" || env.GetTenantId() != c.tenant || cmd.GetPortfolioId() != c.portfolio || !auth.PortfolioEntitled(meta.GetPrincipalPortfolios(), c.portfolio) || meta.GetIssuer() != "user:synthetic-trader" || meta.GetTargetId() != cmd.GetOrderId() || env.GetPartitionKey() != cmd.GetOrderId() {
		return nil, errors.New("synthetic entitlement refused")
	}
	if cmd.GetVenue() != "XSIM" || cmd.GetInstrumentId() != "SYNTH-USD" || cmd.GetSide() != orderpb.Side_SIDE_BUY || cmd.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT || cmd.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_GTC || cmd.GetQuantity().GetCoefficient() <= 0 || cmd.GetLimitPrice().GetCoefficient() <= 0 {
		return nil, errors.New("unsupported synthetic order")
	}
	if c.quote == nil || c.quote.GetInstrumentId() != cmd.GetInstrumentId() || c.quote.GetEventTime().CheckValid() != nil {
		return nil, errors.New("missing synthetic quote")
	}
	effective := c.quote.GetEventTime().AsTime()
	const maxAge = 30 * time.Second
	if effective.After(c.observed) || c.observed.After(now) || !now.Before(effective.Add(maxAge)) {
		return nil, errors.New("synthetic quote outside admission window")
	}
	quotes := mark.New(func() time.Time { return now }, maxAge)
	quoteWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.quote)
	if err != nil {
		return nil, err
	}
	if err = quotes.Handle(ctx, &envelopepb.Envelope{EventType: "market.spot.quote", EventTime: c.quote.GetEventTime()}, quoteWire); err != nil {
		return nil, err
	}
	if quotes.Mark(cmd.GetInstrumentId()) == nil {
		return nil, errors.New("invalid synthetic quote")
	}
	registry := compliance.NewMandateRegistry()
	if err = compliance.ValidateMandate(c.mandate); err != nil {
		return nil, err
	}
	if err = registry.Put(c.mandate); err != nil {
		return nil, err
	}
	active, gov, err := registry.Mandate(ctx, c.tenant, c.portfolio, now)
	if err != nil || gov.NoMandate() || len(active.GetRules()) != 2 || active.GetRules()[0].GetType() != compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE || active.GetRules()[1].GetType() != compliancepb.RuleType_RULE_TYPE_BUYING_POWER {
		return nil, errors.New("synthetic mandate unresolved")
	}
	if c.book == nil || c.book.PortfolioID != c.portfolio || !c.book.CashCompleteness.Vouched() {
		return nil, errors.New("synthetic book unavailable")
	}
	g := compliance.NewPreTradeGate(compliance.NewEngine(nil), compliance.MapBookSource{c.portfolio: c.book}, registry, nil, nil, nil, compliance.WithRequireMandate(true))
	dec, err := g.Evaluate(ctx, compliance.OrderDelta{TenantID: c.tenant, PortfolioID: c.portfolio, InstrumentID: cmd.GetInstrumentId(), OrderID: cmd.GetOrderId(), Issuer: meta.GetIssuer(), Venue: cmd.GetVenue(), SignedQuantity: cmd.GetQuantity(), Price: cmd.GetLimitPrice(), Currency: "USD", AsOf: now})
	if err != nil {
		return nil, err
	}
	res := dec.Result
	if !dec.Allowed || res == nil || res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS || res.GetMandateId() != active.GetMandateId() || res.GetMandateVersion() != active.GetVersion() {
		return nil, errors.New("projected synthetic order refused by mandate")
	}
	mandateWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(active)
	if err != nil {
		return nil, err
	}
	resultWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(res)
	if err != nil {
		return nil, err
	}
	resultID := fmt.Sprintf("%x", sha256.Sum256(resultWire))
	expires := effective.Add(maxAge)
	return []syntheticEvidence{
		{"entitlement", env.GetEventId(), now, now, expires},
		{"market", fmt.Sprintf("%x", sha256.Sum256(quoteWire)), effective, c.observed, expires},
		{"mandate", fmt.Sprintf("%x", sha256.Sum256(mandateWire)), active.GetEffectiveAt().AsTime(), now, expires},
		{"risk", resultID + "/synthetic-gross-leverage", now, now, expires},
		{"compliance", resultID + "/synthetic-buying-power", now, now, expires},
	}, nil
}

func TestSyntheticOrderControlEvidence(t *testing.T) {
	frame := signedSyntheticOrder(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		change func(*syntheticControls)
		want   bool
	}{
		{"fresh", func(*syntheticControls) {}, true},
		{"other-tenant", func(c *syntheticControls) { c.tenant = "second-tenant" }, false},
		{"missing-market", func(c *syntheticControls) { c.quote = nil }, false},
		{"stale-market", func(c *syntheticControls) { c.quote.EventTime = timestamppb.New(now.Add(-31 * time.Second)) }, false},
		{"future-observation", func(c *syntheticControls) { c.observed = now.Add(time.Second) }, false},
		{"future-effective", func(c *syntheticControls) { c.quote.EventTime = timestamppb.New(now.Add(time.Second)) }, false},
		{"no-cash", func(c *syntheticControls) { c.book.Cash = nil }, false},
		{"below-cash-floor", func(c *syntheticControls) {
			c.book.Cash = &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 300}}
		}, false},
		{"risk-limit", func(c *syntheticControls) {
			c.mandate.Rules[0].GetLeverageCap().MaxGrossLeverage = &commonpb.Decimal{Coefficient: 1, Exponent: -1}
		}, false},
		{"missing-mandate", func(c *syntheticControls) { c.mandate = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSyntheticControls(now)
			tc.change(&c)
			rows, err := c.evaluate(t.Context(), frame, now)
			if tc.want {
				if err != nil || len(rows) != 5 {
					t.Fatalf("expected five assessed controls: %v", err)
				}
				return
			}
			if err == nil || len(rows) != 0 {
				t.Fatal("refusal retained synthetic authority")
			}
		})
	}
}
