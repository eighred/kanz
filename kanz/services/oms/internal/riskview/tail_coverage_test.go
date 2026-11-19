package riskview

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestUnpricedTailHoldingMakesPreviouslyKnownVaRUnavailable(t *testing.T) {
	ctx := t.Context()
	prices := store.NewMemory()
	for i, price := range []int64{100, 50, 25} {
		at := t0.AddDate(0, 0, i-2)
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
	p.SetAggregate(domain.AggregateUpdate{AsOf: t0, BaseCurrency: "USD"})
	view := New(WithClock(func() time.Time { return t0 }))

	for _, tc := range []struct {
		mark  *commonpb.Decimal
		known bool
		usd   int64
	}{
		{&commonpb.Decimal{Coefficient: 1000}, true, 500},
		{nil, false, 0},
		{&commonpb.Decimal{}, true, 0},
	} {
		p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: tc.mark}})
		measures := compute.ComputeMeasures(p, reg, []v1.MeasureName{compute.MeasureVaR99})
		fact := publish.ToProtoMeasureSet(measures, []string{"portfolio-revalued"})
		if len(fact.Measures) != 1 || fact.Measures[0].Value == nil {
			t.Fatalf("missing wire VaR: %v", fact)
		}
		if !tc.known && (fact.Measures[0].GetCoverage().GetExcludedCount() == 0 || fact.Measures[0].GetProvenance().GetExcludedGross() != nil) {
			t.Fatalf("unassessed wire VaR: %v", fact.Measures[0])
		}
		payload, err := proto.Marshal(fact)
		if err != nil {
			t.Fatal(err)
		}
		if err := view.Handle(ctx, nil, payload); err != nil {
			t.Fatal(err)
		}
		got, ok := view.Measure("P1", "VaR99")
		if ok != tc.known || (ok && (!got.IsInt() || got.Num().Int64() != tc.usd)) {
			t.Errorf("mark %v: OMS VaR=%v known=%t", tc.mark, got, ok)
		}
	}
}
