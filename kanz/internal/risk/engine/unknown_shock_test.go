package engine_test

import (
	"context"
	"errors"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"reflect"
	"strings"
	"testing"
	"time"
)

type unsupportedShock struct{}

func (unsupportedShock) Description() string { panic("unsupported shock must not be invoked") }

func TestEvaluateScenarioUnknownShockRefusesWithoutChangingState(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-time.Second))
	ctx := context.Background()
	before, err := e.Measures(ctx, v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, shock := range []v1.ScenarioShock{unsupportedShock{}, nil, (*v1.VolShock)(nil)} {
		resp, err := e.EvaluateScenario(ctx, v1.ScenarioRequest{PortfolioID: "PORT-1", Shocks: []v1.ScenarioShock{shock}})
		if !errors.Is(err, v1.ErrScenarioUnresolvable) || !strings.Contains(err.Error(), "unknown_shock_type") || resp.Projected != nil {
			t.Fatalf("response=%+v error=%v", resp, err)
		}
	}
	after, err := e.Measures(ctx, v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Set, after.Set) {
		t.Fatal("refused scenario changed live measures")
	}
}
