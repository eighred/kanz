package scenario_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

type forbiddenRevaluer struct{ t *testing.T }

func (r forbiddenRevaluer) RevalueOption(context.Context, string, time.Time, *commonpb.Money, compute.RevalShocks) (*commonpb.Money, bool) {
	r.t.Fatal("invalid batch reached the revaluer")
	return nil, false
}

type untrustedShock struct{}

func (untrustedShock) Description() string { panic("unknown shock methods must not be called") }

func TestUnknownShockRefusesWholeBatchBeforeValuation(t *testing.T) {
	for _, mode := range []string{"linear", "revaluer", "reval-entry"} {
		for _, size := range []int{0, 2} {
			for name, bad := range map[string]v1.ScenarioShock{
				"custom": untrustedShock{}, "nil": nil,
				"typed-nil": (*v1.PriceShock)(nil), "pointer": &v1.ParallelShift{},
			} {
				t.Run(mode+"/"+name+"/"+string(rune('0'+size)), func(t *testing.T) {
					p := makePortfolio()
					for i := 0; i < size; i++ {
						p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(string(rune('A' + i))), MarketValue: money(100, 0, "USD"), AsOf: baseTime})
					}
					before := compute.ComputeMeasures(p, nil, nil)
					shocks := []v1.ScenarioShock{v1.ParallelShift{Pct: pct(-5, -1)}, bad, bad}
					var got *domain.MeasureSet
					var cov v1.InputCoverage
					switch mode {
					case "linear":
						got, cov = scenario.Evaluate(p, shocks, nil)
					case "revaluer":
						got, cov = scenario.Evaluate(p, shocks, nil, scenario.WithRevaluer(forbiddenRevaluer{t}))
					case "reval-entry":
						got, cov = scenario.EvaluateReval(p, shocks, nil, forbiddenRevaluer{t})
					}
					want := v1.InputCoverage{ExcludedCount: 1, Exclusions: []v1.InputExclusion{{Reason: "unknown_shock_type"}}}
					if got != nil || !reflect.DeepEqual(cov, want) {
						t.Fatalf("got measures=%v coverage=%+v, want nil and %+v", got, cov, want)
					}
					if !reflect.DeepEqual(before, compute.ComputeMeasures(p, nil, nil)) {
						t.Fatal("refused batch mutated the input book")
					}
				})
			}
		}
	}
}
