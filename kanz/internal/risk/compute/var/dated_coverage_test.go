package varmodel_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
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

func TestTailInputsRequireReadableBaseCurrencyMarks(t *testing.T) {
	ctx := t.Context()
	// BAD's non-overlapping dates would destroy A's panel if it participated.
	provider := datedVectors{
		"A":   returnstest.Series("A", asOf, []float64{-.5, -.5}),
		"BAD": returnstest.Series("BAD", asOf.AddDate(0, 0, -10), []float64{-.8, -.9}),
	}
	cfg := varmodel.Config{Draws: 100, Seed: 17}
	a := domain.Position{InstrumentID: "A", MarketValue: money(1000, "USD")}
	foreign := domain.Position{InstrumentID: "EUR_ASSET", MarketValue: money(9, "EUR")}
	noHistory := domain.Position{InstrumentID: "NO_HISTORY", MarketValue: money(-20, "USD")}
	measures := []struct {
		name string
		run  compute.ReturnsMeasure
		want float64
	}{
		{"VaR", varmodel.Historical(cfg), 500},
		{"ES", varmodel.ExpectedShortfall(cfg), 500},
		{"drawdown amount", varmodel.MaxDrawdownAmount(cfg), 1000},
		{"drawdown fraction", varmodel.MaxDrawdownFraction(cfg), 1},
		{"Monte Carlo", varmodel.MonteCarlo(cfg), 500},
	}
	for _, tc := range measures {
		t.Run(tc.name, func(t *testing.T) {
			priced := tc.run(ctx, portfolio("USD", a, foreign, noHistory), provider)
			// Constant return series use a 1e-12 covariance jitter in Monte Carlo.
			// At $1000 its seeded loss also rounds to $500.00.
			if dval(priced.Value) != tc.want || priced.Coverage.Contributed != 1 || priced.Coverage.ExcludedCount != 1 ||
				priced.Provenance.InputDigest == "" || dval(priced.Provenance.ExcludedGross) != 20 {
				t.Fatalf("invalid priced-book control: %+v", priced)
			}
			for _, mark := range []*commonpb.Decimal{nil, {Coefficient: 1, Exponent: 65}} {
				bad := domain.Position{InstrumentID: "BAD", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: mark}}
				mixed := portfolio("USD", a, bad, foreign, noHistory)
				m := tc.run(ctx, mixed, provider)
				wantExclusions := []v1.InputExclusion{
					{InstrumentID: "BAD", Reason: compute.SkipNoMarketValue},
					{InstrumentID: "NO_HISTORY", Reason: varmodel.SkipNoReturns},
					{Reason: "excluded_exposure_unrepresentable"},
				}
				if dval(m.Value) != tc.want || m.Coverage.Contributed != 1 || m.Coverage.ExcludedCount != len(wantExclusions) ||
					!slices.Equal(m.Coverage.Exclusions, wantExclusions) || m.Provenance.ExcludedGross != nil ||
					m.Provenance.InputDigest != priced.Provenance.InputDigest {
					t.Fatalf("mixed book with mark %v: %+v", mark, m)
				}
				currency := mixed.CurrencyExclusions()
				if len(currency) != 1 || currency[0].InstrumentID != foreign.InstrumentID {
					t.Fatalf("currency exclusions: %+v", currency)
				}
				unknown := tc.run(ctx, portfolio("USD", bad), provider)
				if unknown.Value == nil || dval(unknown.Value) != 0 || unknown.Coverage.Contributed != 0 || unknown.Coverage.ExcludedCount != 2 ||
					!slices.Equal(unknown.Coverage.Exclusions, []v1.InputExclusion{wantExclusions[0], wantExclusions[2]}) ||
					unknown.Provenance.ExcludedGross != nil || unknown.Provenance.InputDigest != "" {
					t.Fatalf("unassessed book with mark %v: %+v", mark, unknown)
				}
			}
			zero := tc.run(ctx, portfolio("USD", domain.Position{InstrumentID: "A", MarketValue: money(0, "USD")}), provider)
			if zero.Value == nil || dval(zero.Value) != 0 || zero.Coverage.Contributed != 1 || zero.Coverage.ExcludedCount != 0 ||
				zero.Provenance.ExcludedGross == nil || zero.Provenance.InputDigest == "" {
				t.Fatalf("genuine zero: %+v", zero)
			}
		})
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
