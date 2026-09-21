package fillfact

import (
	"math/big"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func feeFixture(t *testing.T) (*orderpb.Fill, *orderpb.Fill) {
	t.Helper()
	f := &orderpb.Fill{OrderId: "order", FillId: "alias", Venue: "venue", VenueAccountId: "account", VenueExecutionId: "trade", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 2}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(time.Unix(1700000000, 0))}
	digest, err := EconomicDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	g := proto.Clone(f).(*orderpb.Fill)
	g.Fee.Amount = &commonpb.Decimal{Coefficient: 125, Exponent: -2}
	g.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case", FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: "case", Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(time.Unix(1700000001, 0)), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(f.Fee).(*commonpb.Money)}}
	return f, g
}

func TestFeeCorrectionBindsExactEconomicsAndSeparateApprover(t *testing.T) {
	f, g := feeFixture(t)
	delta, err := ApprovedFeeDelta(f, g)
	if err != nil || delta.Cmp(big.NewRat(3, 4)) != 0 {
		t.Fatalf("delta=%v err=%v", delta, err)
	}
	for name, mutate := range map[string]func(*orderpb.Fill){
		"quantity":      func(v *orderpb.Fill) { v.Quantity.Coefficient++ },
		"price":         func(v *orderpb.Fill) { v.Price.Coefficient++ },
		"account":       func(v *orderpb.Fill) { v.VenueAccountId = "other" },
		"currency":      func(v *orderpb.Fill) { v.Fee.CurrencyCode = "EUR" },
		"self approval": func(v *orderpb.Fill) { v.Recovery.FeeApproval.Approver = " MAKER " },
		"baseline":      func(v *orderpb.Fill) { v.Recovery.FeeApproval.PreviousFee.Amount.Coefficient++ },
		"digest":        func(v *orderpb.Fill) { v.Recovery.FeeApproval.PreviousExecutionDigest = strings.Repeat("b", 64) },
		"no change":     func(v *orderpb.Fill) { v.Fee = proto.Clone(f.Fee).(*commonpb.Money) },
		"zero time":     func(v *orderpb.Fill) { v.ExecutedAt = timestamppb.New(time.Unix(0, 0)) },
	} {
		t.Run(name, func(t *testing.T) {
			v := proto.Clone(g).(*orderpb.Fill)
			mutate(v)
			if _, err := ApprovedFeeDelta(f, v); err == nil {
				t.Fatal("unapproved economics accepted")
			}
		})
	}
}

func TestEconomicDigestNormalizesRepresentationButBindsTenantProposal(t *testing.T) {
	f, g := feeFixture(t)
	a, err := EconomicDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	v := proto.Clone(f).(*orderpb.Fill)
	v.FillId = "history-alias"
	v.Price = &commonpb.Decimal{Coefficient: 10000, Exponent: -2}
	v.Recovery = g.Recovery
	b, err := EconomicDigest(v)
	if err != nil || a != b {
		t.Fatalf("digest changed: %v", err)
	}
	p := &orderpb.ExecutionFeeCorrectionProposed{CaseId: "case", Changes: []*orderpb.ExecutionFeeChange{{Previous: f, Revised: g}}}
	a, err = FeeProposalDigest("tenant-a", p)
	if err != nil {
		t.Fatal(err)
	}
	b, err = FeeProposalDigest("tenant-b", p)
	if err != nil || a == b {
		t.Fatal("tenant not bound")
	}
	p.Digest = a
	b, err = FeeProposalDigest("tenant-a", p)
	if err != nil || a != b {
		t.Fatal("self digest changed terms")
	}
	p.Changes[0].Revised.Fee.Amount.Coefficient++
	b, err = FeeProposalDigest("tenant-a", p)
	if err != nil || a == b {
		t.Fatal("fee amount not bound")
	}
}
