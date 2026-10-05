package compute

import (
	"context"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestLiquidityBaseVaRUsesEvaluationContext(t *testing.T) {
	type key struct{}
	p, provider := liqTestBook(t)
	r := DefaultRegistry()
	seen := 0
	r.RegisterScoped(MeasureVaR99, context.WithValue(context.Background(), key{}, 1), func(ctx context.Context) MeasureFunc {
		return func(*domain.Portfolio) v1.Measure {
			seen = ctx.Value(key{}).(int)
			return v1.Measure{Name: MeasureVaR99, Value: &commonpb.Decimal{Coefficient: 100}}
		}
	})
	RegisterLiquidityRisk(context.WithValue(context.Background(), key{}, 2), r, provider, liquidity.DefaultModel(), nil)
	ComputeMeasuresContext(context.WithValue(context.Background(), key{}, 3), p, r, []v1.MeasureName{MeasureLVaR99})
	if seen != 3 {
		t.Fatalf("LVaR base used a different input scope: %d", seen)
	}
	ComputeMeasures(p, r, []v1.MeasureName{MeasureLVaR99})
	if seen != 1 {
		t.Fatalf("legacy VaR binding changed: %d", seen)
	}
}
