package varmodel_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

type datedVectors map[string]returns.Series

func (d datedVectors) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	panic("risk used undated data")
}
func (d datedVectors) DatedReturns(_ context.Context, id string, _ time.Time, _ int) (returns.Series, error) {
	return d[id], nil
}

func TestDatedRiskCoverageAndConcurrentProvenance(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "A", MarketValue: money(1000, "USD")}, domain.Position{InstrumentID: "MISSING", MarketValue: money(-500, "USD")})
	provider := datedVectors{"A": returnstest.Series("A", asOf, []float64{-.1, .2, -.05})}
	for _, measure := range []compute.ReturnsMeasure{varmodel.Historical(varmodel.Config{}), varmodel.MonteCarlo(varmodel.Config{Draws: 100})} {
		first := measure(context.Background(), p, provider)
		if first.Coverage.Contributed != 1 || first.Coverage.ExcludedCount != 1 || dval(first.Provenance.ExcludedGross) != 500 || first.Provenance.InputDigest == "" {
			t.Fatalf("lost excluded exposure: %+v", first)
		}
		digest := first.Provenance.InputDigest
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				m := measure(context.Background(), p, provider)
				if m.Provenance.InputDigest != digest {
					t.Error("concurrent replay changed digest")
				}
				m.Provenance.Params["panel_policy"] = "caller mutation"
			})
		}
		wg.Wait()
		if first.Provenance.InputDigest != digest || first.Provenance.Params["panel_policy"] != returns.IntersectionPolicy {
			t.Fatal("evaluations shared mutable provenance")
		}
	}
}

type undated struct{}

func (undated) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	return []float64{-.9, -.8}, nil
}
func TestDatedRiskDoesNotFallbackToUndatedTailAlignment(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "A", MarketValue: money(1000, "USD")})
	for _, m := range []compute.ReturnsMeasure{varmodel.Historical(varmodel.Config{}), varmodel.MonteCarlo(varmodel.Config{})} {
		got := m(context.Background(), p, undated{})
		if got.Coverage.Contributed != 0 || got.Coverage.ExcludedCount == 0 || dval(got.Value) != 0 {
			t.Fatal("undated provider became a measured risk result")
		}
	}
}

func TestExcludedExposureDoesNotRoundAnUnknownAmountToZero(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "MISSING", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1, Exponent: -12}}})
	tiny := varmodel.Historical(varmodel.Config{})(context.Background(), p, undated{})
	if tiny.Provenance.ExcludedGross == nil || tiny.Provenance.ExcludedGross.Coefficient != 1 || tiny.Provenance.ExcludedGross.Exponent != -12 {
		t.Fatal("representable sub-cent exclusion was lost")
	}
	p.SetPosition(domain.Position{InstrumentID: "LARGE", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1, Exponent: 19}}})
	m := varmodel.Historical(varmodel.Config{})(context.Background(), p, undated{})
	if m.Provenance.ExcludedGross != nil {
		t.Fatal("unrepresentable exclusion was rounded into a reported zero")
	}
	for _, ex := range m.Coverage.Exclusions {
		if ex.Reason == "excluded_exposure_unrepresentable" {
			return
		}
	}
	t.Fatal("unknown excluded exposure was not reported")
}
