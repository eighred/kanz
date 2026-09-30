package corpact

import (
	"testing"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func coupon() *accountingpb.CorporateAction {
	return &accountingpb.CorporateAction{ActionId: "coupon", AnnouncementRevision: 1, ActionType: accountingpb.CorporateActionType_CORPORATE_ACTION_TYPE_COUPON, InstrumentId: "BOND", ExDate: timestamppb.New(day(2)), AnnouncedAt: timestamppb.New(day(25)), PayDate: timestamppb.New(day(23)), PaidAt: timestamppb.New(day(23)), PaymentRef: "custody", CurrencyCode: "USD", IncomePerUnit: &commonpb.Decimal{Coefficient: 3, Exponent: -2}}
}

func TestWireSemiannualCouponExactPeriodicAmount(t *testing.T) {
	m := coupon()
	// Exercise serialized boundary, not only a domain fixture. A 6% annual
	// coupon paid semiannually supplies 0.03 per face for this period.
	wire, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var received accountingpb.CorporateAction
	if err := proto.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	e, err := FromProto("PF", &received)
	if err != nil {
		t.Fatal(err)
	}
	hold := buy("face", "BOND", "1000", "1", 1, 1)
	hold.Cash = nil
	b := ledger.ReplayAsOf("PF", []*ledger.Event{hold, e}, day(22), day(25))
	if b.CashBalance("USD").Sign() != 0 || b.AccruedBalance("USD").Cmp(dec.Rat("30")) != 0 {
		t.Fatal("coupon was not accrued exactly")
	}
	b = ledger.Replay("PF", []*ledger.Event{hold, e})
	if b.CashBalance("USD").Cmp(dec.Rat("30")) != 0 || b.AccruedBalance("USD").Sign() != 0 {
		t.Fatal("periodic coupon was annualized or paid twice")
	}
}

func TestWireRefusesAmbiguousAndMissingTerms(t *testing.T) {
	for name, mutate := range map[string]func(*accountingpb.CorporateAction){
		"annual legacy rate": func(m *accountingpb.CorporateAction) { m.CouponRate = .06 },      //nolint:staticcheck // Verify deprecated inputs are refused.
		"legacy dividend":    func(m *accountingpb.CorporateAction) { m.DividendPerShare = .1 }, //nolint:staticcheck // Verify deprecated inputs are refused.
		"legacy ratio":       func(m *accountingpb.CorporateAction) { m.Ratio = 2 },             //nolint:staticcheck // Verify deprecated inputs are refused.
		"missing revision":   func(m *accountingpb.CorporateAction) { m.AnnouncementRevision = 0 },
		"missing pay date":   func(m *accountingpb.CorporateAction) { m.PayDate = nil },
		"missing amount":     func(m *accountingpb.CorporateAction) { m.IncomePerUnit = nil },
		"huge exponent":      func(m *accountingpb.CorporateAction) { m.IncomePerUnit.Exponent = 9999999 },
		"unknown kind":       func(m *accountingpb.CorporateAction) { m.ActionType = 99 },
		"invalid timestamp":  func(m *accountingpb.CorporateAction) { m.ExDate.Nanos = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			m := coupon()
			mutate(m)
			if _, err := FromProto("PF", m); err == nil {
				t.Fatal("accepted ambiguous action")
			}
		})
	}
}

func TestToEntryOwnsExactTermsAndRevisionIdentity(t *testing.T) {
	c := CorporateAction{ActionID: "split", PortfolioID: "PF", Kind: Split, InstrumentID: "EQ", Ratio: dec.Rat("1.5"), ExDate: day(2), AnnouncedAt: day(1), ActionLifecycle: ledger.ActionLifecycle{Revision: 1}}
	e := entry(t, c)
	c.Ratio.SetInt64(2)
	if e.Action.Ratio.Cmp(dec.Rat("1.5")) != 0 {
		t.Fatal("entry aliases mutable ratio")
	}
	c.Revision = 2
	c.AnnouncedAt = day(3)
	if e.EntryID == entry(t, c).EntryID {
		t.Fatal("amendment shares journal identity")
	}
	c.PortfolioID = "PF2"
	if e.EntryID == entry(t, c).EntryID {
		t.Fatal("identity is not portfolio scoped")
	}
}
