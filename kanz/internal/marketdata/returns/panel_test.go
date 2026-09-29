package returns_test

import (
	"context"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
)

type dated map[string]returns.Series

func (d dated) DatedReturns(_ context.Context, id string, _ time.Time, _ int) (returns.Series, error) {
	return d[id], nil
}

var horizon = time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

func TestPanelUsesExactIntervalsAndCanonicalOrder(t *testing.T) {
	a := returnstest.Series("A", horizon, []float64{1, 2, 3, 4, 5})
	b := returnstest.Series("B", horizon, []float64{10, 20, 30, 40, 50})
	b.Observations = append(b.Observations[:1:1], b.Observations[2:]...) // missing a close interval
	p, err := returns.Load(context.Background(), dated{"A": a, "B": b}, []string{"B", "A"}, horizon, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Values, [][]float64{{1, 3, 4, 5}, {10, 30, 40, 50}}) || p.Dropped != 1 || p.Contiguous() {
		t.Fatalf("misaligned panel: %+v", p)
	}
	q, err := returns.Load(context.Background(), dated{"A": a, "B": b}, []string{"A", "B", "A"}, horizon.In(time.FixedZone("other", 3600)), 0)
	if err != nil || p.Digest != q.Digest {
		t.Fatalf("order/timezone changed digest: %s %s %v", p.Digest, q.Digest, err)
	}
	b.Observations[0].EndRevision = "corrected"
	q, err = returns.Load(context.Background(), dated{"A": a, "B": b}, []string{"A", "B"}, horizon, 0)
	if err != nil || p.Digest == q.Digest {
		t.Fatal("source revision did not change artifact")
	}
}

func TestPanelRejectsUnprovenSeries(t *testing.T) {
	mutations := map[string]func(*returns.Series){
		"nan":                func(s *returns.Series) { s.Observations[0].Value = math.NaN() },
		"inf":                func(s *returns.Series) { s.Observations[0].Value = math.Inf(1) },
		"duplicate":          func(s *returns.Series) { s.Observations[1] = s.Observations[0] },
		"reverse":            func(s *returns.Series) { s.Observations[0], s.Observations[1] = s.Observations[1], s.Observations[0] },
		"different duration": func(s *returns.Series) { s.Observations[0].Start = s.Observations[0].Start.Add(-time.Hour) },
		"future correction":  func(s *returns.Series) { s.Observations[0].StartKnowledge = horizon.Add(time.Second) },
		"future effective":   func(s *returns.Series) { s.Observations[1].End = horizon.Add(time.Hour) },
		"missing revision":   func(s *returns.Series) { s.Observations[0].StartRevision = "" },
		"unknown calendar":   func(s *returns.Series) { s.Calendar = "" },
		"wrong instrument":   func(s *returns.Series) { s.InstrumentID = "B" },
		"invalid convention": func(s *returns.Series) { s.Method = 99 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := returnstest.Series("A", horizon, []float64{.1, -.1})
			mutate(&s)
			p, err := returns.Load(context.Background(), dated{"A": s}, []string{"A"}, horizon, 0)
			if err == nil || len(p.Missing) != 1 {
				t.Fatalf("unproven panel accepted: %+v %v", p, err)
			}
		})
	}
	if _, err := returns.Load(context.Background(), struct{}{}, []string{"A"}, horizon, 0); err == nil {
		t.Fatal("undated provider accepted")
	}
}

func TestPanelNeverMatchesOnlyTheEndOrMutatesProvider(t *testing.T) {
	a := returnstest.Series("A", horizon, []float64{.1, .2, .3})
	b := returnstest.Series("B", horizon.Add(-time.Hour), []float64{.1, .2, .3})
	if _, err := returns.Load(context.Background(), dated{"A": a, "B": b}, []string{"A", "B"}, horizon, 0); err == nil {
		t.Fatal("different closes aligned")
	}
	zone := time.FixedZone("source", 3600)
	for i := range a.Observations {
		a.Observations[i].Start = a.Observations[i].Start.In(zone)
		a.Observations[i].End = a.Observations[i].End.In(zone)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := returns.Load(context.Background(), dated{"A": a}, []string{"A"}, horizon, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if a.Observations[0].Start.Location() != zone {
		t.Fatal("panel mutated provider data")
	}
}

func FuzzPanelRejectsNonFinite(f *testing.F) {
	f.Add(.1)
	f.Add(math.NaN())
	f.Add(math.Inf(1))
	f.Fuzz(func(t *testing.T, v float64) {
		s := returnstest.Series("A", horizon, []float64{v, .2})
		p, err := returns.Load(context.Background(), dated{"A": s}, []string{"A"}, horizon, 0)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			if err == nil {
				t.Fatal("invalid calibration accepted")
			}
		} else if err != nil || len(p.Intervals) != 2 {
			t.Fatalf("finite panel refused: %v", err)
		}
	})
}
