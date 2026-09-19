package audit

import (
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func discrepancyBalance() *accountingpb.BalanceReconciled {
	return &accountingpb.BalanceReconciled{PortfolioId: "tenant-legacy", Venue: "BINANCE", Asset: "USD", Expected: &commonpb.Decimal{}, Actual: &commonpb.Decimal{Coefficient: 1, Exponent: -9}, Delta: &commonpb.Decimal{Coefficient: 1, Exponent: -9}, DetectedAt: timestamppb.New(time.Unix(1700000000, 0))}
}

func TestDiscrepancyEvidenceNeverAuthorizesMoney(t *testing.T) {
	env := &envelopepb.Envelope{EventType: "accounting.balance.reconciled", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	for _, tc := range []struct {
		name   string
		change func(*accountingpb.BalanceReconciled)
		valid  bool
	}{
		{"exact", func(*accountingpb.BalanceReconciled) {}, true},
		{"missing expected", func(m *accountingpb.BalanceReconciled) { m.Expected = nil }, false},
		{"false delta", func(m *accountingpb.BalanceReconciled) { m.Delta.Coefficient = 2 }, false},
		{"unsafe exponent", func(m *accountingpb.BalanceReconciled) { m.Actual.Exponent = 1000000 }, false},
		{"no observation time", func(m *accountingpb.BalanceReconciled) { m.DetectedAt = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := discrepancyBalance()
			tc.change(m)
			b, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			c := classify(env, b)
			if c.kind != KindVenueDiscrepancy || c.attrs["disposition"] != "investigate" || c.attrs["scope_status"] != "unverified" || len(c.attrs["payload_sha256"]) != 64 {
				t.Fatalf("lost evidence: %+v", c)
			}
			if (c.attrs["evidence_status"] == "observed") != tc.valid {
				t.Fatalf("wrong validity: %+v", c)
			}
			if tc.valid && (c.attrs["actual"] != "1/1000000000" || c.attrs["reported_portfolio_id"] != "tenant-legacy") {
				t.Fatalf("invented/rounded scope or money: %+v", c)
			}
		})
	}
	c := classify(env, []byte{255})
	if c.kind != KindVenueDiscrepancy || c.attrs["evidence_status"] != "invalid" {
		t.Fatal("malformed discrepancy disappeared into generic archive")
	}
}

func TestTerminalDiscrepancyRemainsAnObservation(t *testing.T) {
	state := &orderpb.OrderState{OrderId: "order", Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, FilledQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: -9}}
	msg := &orderpb.StateHealed{OrderId: "order", Venue: "OKX", State: state, Reason: "private free-form detail", DetectedAt: timestamppb.Now()}
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	c := classify(&envelopepb.Envelope{EventType: "order.order.healed", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}, b)
	if c.attrs["order_status"] != "FILLED" || c.attrs["filled_quantity"] != "1/1000000000" || c.attrs["disposition"] != "investigate" {
		t.Fatalf("wrong terminal evidence: %+v", c)
	}
	if _, ok := c.attrs["reason"]; ok {
		t.Fatal("unbounded free-form detail exposed")
	}
}
