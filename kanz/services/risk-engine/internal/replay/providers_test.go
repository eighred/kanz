package replay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

type datedSource struct {
	calls   int
	changed bool
}

func (s *datedSource) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	panic("dated contract required")
}
func (s *datedSource) DatedReturns(_ context.Context, id string, at time.Time, _ int) (returns.Series, error) {
	s.calls++
	series := returns.Series{InstrumentID: id, Calendar: returns.ContinuousUTC, Currency: "USD", Method: returns.ReturnSimple, Kind: store.PriceKindClose}
	for i, value := range []float64{-.10, -.05, 0, .05, .10} {
		end := at.AddDate(0, 0, i-5)
		if s.changed {
			value *= 2
		}
		series.Observations = append(series.Observations, returns.Observation{Interval: returns.Interval{Start: end.AddDate(0, 0, -1), End: end}, Value: value, StartKnowledge: end, EndKnowledge: end, StartRevision: "start", EndRevision: "end"})
	}
	return series, nil
}

func TestHistoricalRiskUsesRetainedPanelAfterSourceChanges(t *testing.T) {
	ctx := context.Background()
	source := new(datedSource)
	sink := new(recordSink)
	r := compute.DefaultRegistry()
	varmodel.Register(ctx, r, Returns{Source: source}, varmodel.Config{})
	e, err := New(r, sink, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	result, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	measure, _ := result.Measures.Lookup(compute.MeasureVaR99)
	if measure.Value.GetCoefficient() == 0 || len(result.Measures.UnresolvedMeasures()) != 0 {
		t.Fatalf("fixture did not price: %+v", measure)
	}
	if source.calls != 1 {
		t.Fatalf("same panel fetched %d times during evaluation", source.calls)
	}
	source.changed = true
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 999999}}})
	if _, err := reconstruct(ctx, sink.record); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Fatal("replay consulted live source")
	}
	var m manifest
	if err := json.Unmarshal(sink.record.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	for key := range m.Inputs {
		delete(m.Inputs, key)
	}
	bad := sink.record
	bad.Manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	bad.Digest = manifestDigest(bad.Manifest)
	if _, err := reconstruct(ctx, bad); err == nil {
		t.Fatal("missing return panel accepted")
	}
}

type bondSource struct {
	spec   compute.BondSpec
	calls  int
	status compute.TermsResolution
}

func (s *bondSource) BondTerms(context.Context, string, time.Time) (compute.BondSpec, compute.TermsResolution) {
	s.calls++
	return s.spec, s.status
}

type curveSource struct {
	value *curve.Curve
	calls int
}

func (s *curveSource) Curve(context.Context, string, time.Time) (*curve.Curve, bool) {
	s.calls++
	return s.value, s.value != nil
}

func TestFixedIncomeReplaysBeyondCacheHorizon(t *testing.T) {
	ctx := context.Background()
	p := testPortfolio()
	p.SetPosition(domain.Position{InstrumentID: "A", Quantity: &commonpb.Decimal{Coefficient: 10}, MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1000}}})
	c, err := curve.NewZeroCurve([]float64{1, 5, 10}, []float64{.031234567890123, .04, .045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	terms := &bondSource{spec: compute.BondSpec{Face: 100, CouponRate: .05, Frequency: 2, Issue: p.AsOf().AddDate(-1, 0, 0), Maturity: p.AsOf().AddDate(7, 0, 0), DayCount: pricing.Thirty360, Currency: "USD"}, status: compute.TermsResolved}
	curves := &curveSource{value: c}
	r := compute.DefaultRegistry()
	compute.RegisterFIRisk(ctx, r, compute.FIProviders{Terms: Bonds{Source: terms}, Curve: Curves{Source: curves}})
	sink := new(recordSink)
	e, err := New(r, sink, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := result.Measures.Lookup(compute.MeasureDV01)
	if m.Value.GetCoefficient() == 0 {
		t.Fatal("fixture has no rate sensitivity")
	}
	if terms.calls != 1 || curves.calls != 1 {
		t.Fatal("providers not frozen per evaluation")
	}
	curves.value = nil
	terms.status = compute.TermsUnknown
	if _, err := e.Replay(ctx, v1.PortfolioID("book"), p.AsOf().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if terms.calls != 1 || curves.calls != 1 {
		t.Fatal("replay used current pricing inputs")
	}
}
