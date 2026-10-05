package compute_test

import (
	"context"
	"sync"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestScopedMeasureContextsRemainIndependent(t *testing.T) {
	type key struct{}
	r := compute.NewRegistry()
	r.RegisterScoped("scoped", context.WithValue(context.Background(), key{}, int64(99)), func(ctx context.Context) compute.MeasureFunc {
		return func(*domain.Portfolio) v1.Measure {
			return v1.Measure{Name: "scoped", Value: &commonpb.Decimal{Coefficient: ctx.Value(key{}).(int64)}}
		}
	})
	var wg sync.WaitGroup
	for i := int64(0); i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := domain.NewPortfolio("p", "USD")
			m, _ := compute.ComputeMeasuresContext(context.WithValue(context.Background(), key{}, i), p, r, nil).Lookup("scoped")
			if m.Value.Coefficient != i {
				t.Errorf("context leaked: got %d want %d", m.Value.Coefficient, i)
			}
		}()
	}
	wg.Wait()
	parameters := map[string]string{"window": "17"}
	r.SetParameters("scoped", parameters)
	parameters["window"] = "changed"
	stored, ok := r.Parameters("scoped")
	if !ok || stored["window"] != "17" {
		t.Fatal("mutable configuration escaped")
	}
	stored["window"] = "changed"
	stored, _ = r.Parameters("scoped")
	if stored["window"] != "17" {
		t.Fatal("returned configuration mutated registry")
	}
	p := domain.NewPortfolio("p", "USD")
	m, _ := compute.ComputeMeasures(p, r, nil).Lookup("scoped")
	if m.Value.Coefficient != 99 {
		t.Fatal("legacy binding changed")
	}
	r.Register("scoped", func(*domain.Portfolio) v1.Measure {
		return v1.Measure{Name: "scoped", Value: &commonpb.Decimal{Coefficient: 123}}
	})
	if _, ok := r.Parameters("scoped"); ok {
		t.Fatal("replaced method retained stale parameters")
	}
	m, _ = compute.ComputeMeasuresContext(context.Background(), p, r, nil).Lookup("scoped")
	if m.Value.Coefficient != 123 {
		t.Fatal("stale context factory survived replacement")
	}
}
