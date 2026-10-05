package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
)

// Fault injection at the durable boundary; PG and JetStream are exercised by
// the service replay package's end-to-end tests.
type controlledEvaluator struct {
	ready      atomic.Bool
	calls      atomic.Int64
	historical *domain.Portfolio
}

func (e *controlledEvaluator) Compute(_ context.Context, p *domain.Portfolio, _ factor.Classifier) (engine.Evaluation, error) {
	e.calls.Add(1)
	if !e.ready.Load() {
		return engine.Evaluation{}, errors.New("retention unavailable")
	}
	return engine.Evaluation{Exposure: compute.ComputeExposure(p), Measures: compute.ComputeMeasures(p, nil, nil)}, nil
}
func (e *controlledEvaluator) Replay(context.Context, v1.PortfolioID, time.Time) (engine.Evaluation, error) {
	if e.historical == nil {
		return engine.Evaluation{}, v1.ErrHistoryUnavailable
	}
	return engine.Evaluation{Exposure: compute.ComputeExposure(e.historical), Measures: compute.ComputeMeasures(e.historical, nil, nil)}, nil
}

func TestDurableEvaluationFailurePreventsCacheAndEmitAndRetries(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now())
	eval := new(controlledEvaluator)
	cache := risk.NewCache()
	cc := newEmitCounter()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, newPublisher(t, cc), time.Millisecond, discard(), engine.WithRecomputeEvaluator(eval), engine.WithEmitRetryInterval(5*time.Millisecond))
	defer r.Close()
	r.Trigger("PORT-1")
	waitFor(t, "failed evaluation attempted", func() bool { return eval.calls.Load() > 0 })
	if _, ok := cache.LookupMeasures("PORT-1"); ok {
		t.Fatal("unretained measures cached")
	}
	if cc.count(publish.EventTypeMeasuresComputed) != 0 || cc.count(publish.EventTypeExposureRecomputed) != 0 {
		t.Fatal("unretained risk published")
	}
	eval.ready.Store(true)
	waitFor(t, "quiet portfolio retried", func() bool { return cc.count(publish.EventTypeMeasuresComputed) > 0 })
	if _, ok := cache.LookupMeasures("PORT-1"); !ok {
		t.Fatal("successful retry not cached")
	}
}

func TestAsOfUsesDurableEvaluatorWithoutReadingOrCachingLiveState(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now().Add(-time.Hour))
	old, _ := store.Snapshot("PORT-1")
	eval := &controlledEvaluator{historical: old}
	cache := risk.NewCache()
	query := engine.New(state.NewStore(), compute.DefaultRegistry(), cache, risk.NewDetector(), engine.WithEvaluator(eval))
	response, err := query.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1", AsOf: time.Now(), Measures: []v1.MeasureName{compute.MeasureGrossExposure}})
	if err != nil {
		t.Fatal(err)
	}
	measure, ok := response.Set.Lookup(compute.MeasureGrossExposure)
	if !ok || measure.Value.Coefficient != 1000 || !response.AsOf.Equal(old.AsOf()) {
		t.Fatal("historical result changed")
	}
	if _, ok := cache.LookupMeasures("PORT-1"); ok {
		t.Fatal("historical result entered current cache")
	}
	if eval.calls.Load() != 0 {
		t.Fatal("historical query used live calculation")
	}
	eval.historical = nil
	if _, err := query.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1", AsOf: time.Now()}); !errors.Is(err, v1.ErrHistoryUnavailable) {
		t.Fatalf("missing historical input: %v", err)
	}
}
