package app_test

import (
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/prediction"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func publishedPortfolioRisk(t *testing.T, ms *domain.MeasureSet) *inferencepb.FeatureVector {
	t.Helper()
	capture := newCapture()
	producer, err := bus.NewProducer(capture, bus.ProducerConfig{Source: "risk-engine/test", ProducerVersion: "v1", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := prediction.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}
	app.PublishFeaturesOn(pub, discardLogger())(t.Context(), ms)
	if len(capture.out) == 0 {
		return nil
	}
	messages := capture.out[prediction.EventTypeFeatureComputed]
	if len(capture.out) != 1 || len(messages) != 1 {
		t.Fatalf("expected one feature FACT, got %v", capture.out)
	}
	env, payload, err := bus.Unframe(messages[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	var fv inferencepb.FeatureVector
	if err := proto.Unmarshal(payload, &fv); err != nil {
		t.Fatal(err)
	}
	return &fv
}

func TestPublishFeaturesOnHistoricalCoverage(t *testing.T) {
	asOf := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	prices := store.NewMemory()
	// Both instruments have constant closes and two genuine daily returns.
	// Their trading dates do not overlap, so together they have no joint panel.
	for id, end := range map[string]time.Time{"FLAT": asOf, "EARLIER": asOf.AddDate(0, 0, -4)} {
		for i := -2; i <= 0; i++ {
			at := end.AddDate(0, 0, i)
			if err := prices.Put(t.Context(), []store.Observation{{
				InstrumentID: id, ObservationTime: at, KnowledgeTime: at, Kind: store.PriceKindClose,
				CurrencyCode: "USD", Price: &commonpb.Decimal{Coefficient: 100},
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := compute.DefaultRegistry()
	varmodel.Register(t.Context(), reg, returns.NewStoreReturnsProvider(prices, returns.ReturnsConfig{Method: returns.ReturnSimple}), varmodel.Config{})
	for _, tc := range []struct {
		name         string
		additional   domain.Position
		contributed  int
		exclusions   []v1.InputExclusion
		currencyGaps int
		wantFact     bool
	}{
		{name: "complete dated zero", contributed: 1, wantFact: true},
		{name: "whole evaluation without common dates", additional: domain.Position{InstrumentID: "EARLIER", MarketValue: money(200, "USD")},
			exclusions: []v1.InputExclusion{{Reason: varmodel.SkipInsufficientHistory}}},
		{name: "one missing leg", additional: domain.Position{InstrumentID: "MISSING", MarketValue: money(200, "USD")}, contributed: 1,
			exclusions: []v1.InputExclusion{{InstrumentID: "MISSING", Reason: varmodel.SkipNoReturns}}},
		{name: "base currency excludes a holding from both inputs", additional: domain.Position{InstrumentID: "FOREIGN", MarketValue: money(200, "EUR")},
			contributed: 1, currencyGaps: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := domain.NewPortfolio("fund", "USD")
			p.SetAggregate(domain.AggregateUpdate{BaseCurrency: "USD", AsOf: asOf})
			p.SetPosition(domain.Position{InstrumentID: "FLAT", MarketValue: money(100, "USD")})
			if tc.additional.InstrumentID != "" {
				p.SetPosition(tc.additional)
			}
			ms := compute.ComputeMeasures(p, reg, []v1.MeasureName{compute.MeasureGrossExposure, compute.MeasureVaR99})
			tail, ok := ms.Lookup(compute.MeasureVaR99)
			if !ok || tail.Value == nil || tail.Value.Coefficient != 0 || tail.Coverage.Contributed != tc.contributed ||
				tail.Coverage.ExcludedCount != len(tc.exclusions) || !slices.Equal(tail.Coverage.Exclusions, tc.exclusions) ||
				len(ms.CurrencyExclusions()) != tc.currencyGaps {
				t.Fatalf("unexpected tail or portfolio coverage: %+v, currency exclusions %v", tail, ms.CurrencyExclusions())
			}
			got := publishedPortfolioRisk(t, ms)
			if !tc.wantFact {
				if got != nil {
					t.Fatalf("published a feature FACT for an incomplete book: %v", got)
				}
				return
			}
			want := &inferencepb.FeatureVector{
				SubjectId: "fund", FeatureSetRef: "portfolio-risk:1", AsOf: timestamppb.New(asOf),
				Values: map[string]*inferencepb.FeatureValue{
					"GrossExposure": {Value: &inferencepb.FeatureValue_Scalar{Scalar: 100}},
					"VaR99":         {Value: &inferencepb.FeatureValue_Scalar{Scalar: 0}},
				},
			}
			if !proto.Equal(got, want) {
				t.Fatalf("feature FACT = %v, want %v", got, want)
			}
		})
	}
}

func TestPublishFeaturesOnLegacyMeasureCoverage(t *testing.T) {
	// Legacy measures have no model provenance. The exclusion count remains
	// authoritative even when the bounded sample contains no details.
	for _, tc := range []struct {
		excluded v1.MeasureName
		wantFact bool
	}{
		{"", true}, {"GrossExposure", false}, {"VaR99", false}, {"Delta", true},
	} {
		t.Run("excluded="+string(tc.excluded), func(t *testing.T) {
			inputs := map[v1.MeasureName]v1.Measure{}
			for _, name := range []v1.MeasureName{"GrossExposure", "VaR99", "Delta"} {
				m := v1.Measure{Name: name, Value: &commonpb.Decimal{Coefficient: 10}}
				if name == tc.excluded {
					m.Coverage.ExcludedCount = 1
				}
				inputs[name] = m
			}
			ms := domain.NewMeasureSet("fund", time.Unix(100, 0), inputs)
			got := publishedPortfolioRisk(t, ms)
			if (got != nil) != tc.wantFact {
				t.Fatalf("published = %v, want feature FACT = %t", got, tc.wantFact)
			}
		})
	}
}

func TestFeaturePublicationForUnvaluedBaseCurrencyPositions(t *testing.T) {
	at := timestamppb.New(time.Unix(100, 0))
	for _, tc := range []struct {
		name  string
		first *commonpb.Money
		other bool
		want  bool
	}{
		{"missing", &commonpb.Money{CurrencyCode: "USD"}, false, false},
		{"missing alongside a measured holding", &commonpb.Money{CurrencyCode: "USD"}, true, false},
		{"observed zero", money(0, "USD"), false, true},
		{"observed zero alongside a measured holding", money(0, "USD"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{PortfolioId: "fund", BaseCurrency: "USD", AsOf: at},
				Positions: []*domainpb.PositionState{{PortfolioId: "fund", InstrumentId: "A", MarketValue: tc.first, AsOf: at}}}
			if tc.other {
				in.Positions = append(in.Positions, &domainpb.PositionState{PortfolioId: "fund", InstrumentId: "B", MarketValue: money(100, "USD"), AsOf: at})
			}
			st := state.NewStore()
			if err := st.ApplyPortfolioSnapshot(t.Context(), &envelopepb.Envelope{IdempotencyKey: "source"}, in); err != nil {
				t.Fatal(err)
			}
			p, ok := st.Snapshot("fund")
			if !ok {
				t.Fatal("missing portfolio")
			}
			ms := compute.ComputeMeasuresContext(t.Context(), p, compute.DefaultRegistry(), []v1.MeasureName{compute.MeasureGrossExposure, compute.MeasureVaR99})
			got := publishedPortfolioRisk(t, ms)
			if !tc.want {
				if got != nil {
					t.Fatalf("published absent base-currency amount as a measured feature: %v", got.GetValues())
				}
				wantCount := 0
				if tc.other {
					wantCount = 1
				}
				for _, n := range []v1.MeasureName{compute.MeasureGrossExposure, compute.MeasureVaR99} {
					m, _ := ms.Lookup(n)
					if m.Coverage.Contributed != wantCount || m.Coverage.ExcludedCount != 1 || !slices.Equal(m.Coverage.Exclusions, []v1.InputExclusion{{InstrumentID: "A", Reason: compute.SkipNoMarketValue}}) {
						t.Fatalf("%s missing mark coverage %+v", n, m.Coverage)
					}
				}
				return
			}
			wantGross, wantVar := 0., 0.
			if tc.other {
				wantGross, wantVar = 100, 1
			}
			want := &inferencepb.FeatureVector{SubjectId: "fund", FeatureSetRef: "portfolio-risk:1", AsOf: at,
				Values: map[string]*inferencepb.FeatureValue{
					"GrossExposure": {Value: &inferencepb.FeatureValue_Scalar{Scalar: wantGross}},
					"VaR99":         {Value: &inferencepb.FeatureValue_Scalar{Scalar: wantVar}},
				},
			}
			if !proto.Equal(got, want) {
				t.Fatalf("complete feature %v, want %v", got, want)
			}
		})
	}
}
