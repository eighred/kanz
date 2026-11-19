package publish_test

import (
	"testing"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/test/contract/compatibility"
	"google.golang.org/protobuf/proto"
)

func TestUnpricedTailHoldingPublishesOMSWireFixture(t *testing.T) {
	ctx := t.Context()
	want := compatibility.UnpricedTailVaR()
	asOf := want.AsOf.AsTime()
	prices := store.NewMemory()
	for i, price := range []int64{100, 50, 25} {
		at := asOf.AddDate(0, 0, i-2)
		if err := prices.Put(ctx, []store.Observation{{
			InstrumentID: "A", ObservationTime: at, KnowledgeTime: at, Kind: store.PriceKindClose,
			Price: &commonpb.Decimal{Coefficient: price},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	reg := compute.NewRegistry()
	varmodel.Register(ctx, reg, returns.NewStoreReturnsProvider(prices, returns.ReturnsConfig{Method: returns.ReturnSimple}), varmodel.Config{})
	p := domain.NewPortfolio("P1", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: "USD"})
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{CurrencyCode: "USD"}})
	measures := compute.ComputeMeasures(p, reg, []v1.MeasureName{compute.MeasureVaR99})
	pub, client := newPublisher(t)
	if err := pub.EmitMeasures(ctx, measures, []string{"portfolio-revalued"}); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) != 1 || client.sent[0].Subject != publish.EventTypeMeasuresComputed {
		t.Fatalf("expected one measures FACT, got %v", client.sent)
	}
	env, payload, err := bus.Unframe(client.sent[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	var got domainpb.RiskMeasureSet
	if err := proto.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&got, want) {
		t.Fatalf("published payload differs from OMS fixture:\ngot  %v\nwant %v", &got, want)
	}
}
