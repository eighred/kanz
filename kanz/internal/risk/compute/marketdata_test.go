package compute

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func d(n int) time.Time { return base.AddDate(0, 0, n) }

// fakeProvider records the args it was bound-and-called with. The concrete
// StoreReturnsProvider (and the return math it exercises) moved to
// internal/marketdata/returns; here we only need a stub that satisfies the
// ReturnsProvider interface compute declares, to prove the BindReturns seam.
type fakeProvider struct {
	ret      []float64
	gotInst  string
	gotAsOf  time.Time
	gotCtxOK bool
}

func (f *fakeProvider) Returns(ctx context.Context, inst string, asOf time.Time, _ int) ([]float64, error) {
	f.gotInst = inst
	f.gotAsOf = asOf
	f.gotCtxOK = ctx.Value(ctxProbe{}) == "v"
	return f.ret, nil
}

type ctxProbe struct{}

// TestBindReturns proves the seam: a ReturnsMeasure closed over a provider +
// ctx becomes a plain MeasureFunc, and both the provider and the captured ctx
// reach the computation.
func TestBindReturns(t *testing.T) {
	fp := &fakeProvider{ret: []float64{-0.05, 0.02, -0.07}}
	ctx := context.WithValue(context.Background(), ctxProbe{}, "v")

	// A ReturnsMeasure that reports how many returns the provider yielded for the
	// portfolio's single instrument as of p.AsOf().
	measure := func(ctx context.Context, p *domain.Portfolio, rp ReturnsProvider) v1.Measure {
		r, _ := rp.Returns(ctx, "AAPL", p.AsOf(), 0)
		return v1.Measure{Name: "Probe", Value: &commonpb.Decimal{Coefficient: int64(len(r)), Exponent: 0}}
	}

	var fn MeasureFunc = BindReturns(ctx, fp, measure)

	p := domain.NewPortfolio(v1.PortfolioID("P1"), domain.CurrencyCode("USD"))
	p.SetAggregate(domain.AggregateUpdate{AsOf: d(7)})

	m := fn(p)
	if m.Value.Coefficient != 3 {
		t.Fatalf("bound measure should see 3 returns, got %d", m.Value.Coefficient)
	}
	if fp.gotInst != "AAPL" || !fp.gotAsOf.Equal(d(7)) {
		t.Fatalf("provider args not threaded: inst=%q asOf=%v", fp.gotInst, fp.gotAsOf)
	}
	if !fp.gotCtxOK {
		t.Fatal("captured ctx did not reach the provider")
	}
}
