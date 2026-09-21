package fillfact

import (
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func TestExecutionIdentitySeparatesCollateralAndPreservesLegacy(t *testing.T) {
	f := &orderpb.Fill{FillId: "legacy", Venue: "venue", VenueAccountId: "account", InstrumentId: "instrument", VenueExecutionId: "trade"}
	first := ExecutionKey(f)
	if first == "legacy" || first == "" {
		t.Fatal(first)
	}
	for _, mutate := range []func(*orderpb.Fill){
		func(g *orderpb.Fill) { g.Venue = "another" },
		func(g *orderpb.Fill) { g.VenueAccountId = "another" },
		func(g *orderpb.Fill) { g.InstrumentId = "another" },
		func(g *orderpb.Fill) { g.VenueExecutionId = "another" },
	} {
		g := proto.Clone(f).(*orderpb.Fill)
		mutate(g)
		if ExecutionKey(g) == first {
			t.Fatal("different execution collided")
		}
	}
	f.FillId = "new-alias"
	if ExecutionKey(f) != first {
		t.Fatal("transport alias changed economic identity")
	}
	f.VenueAccountId = ""
	if ExecutionKey(f) != f.FillId {
		t.Fatal("legacy identity renamed without proof")
	}
}

func TestSameExecutionComparesExactEconomics(t *testing.T) {
	f := &orderpb.Fill{FillId: "old", Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: -2}, CurrencyCode: "USD"}}
	g := proto.Clone(f).(*orderpb.Fill)
	g.FillId = "new"
	g.Price = &commonpb.Decimal{Coefficient: 10000, Exponent: -2}
	g.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case"}
	if !SameExecution(f, g) {
		t.Fatal("representation or provenance changed economic equality")
	}
	g.Fee.Amount.Coefficient++
	if SameExecution(f, g) {
		t.Fatal("fee correction silently deduplicated")
	}
}
