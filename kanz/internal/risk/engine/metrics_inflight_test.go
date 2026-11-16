package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type overlappingInflightGauge struct {
	prometheus.Gauge
	firstEntered chan struct{}
	releaseFirst chan struct{}
	zeroWritten  chan struct{}
}

func (g *overlappingInflightGauge) Set(value float64) {
	if value == 1 {
		close(g.firstEntered)
		<-g.releaseFirst
	}
	g.Gauge.Set(value)
	if value == 0 {
		close(g.zeroWritten)
	}
}

func waitInflightPublication(t *testing.T, done <-chan struct{}, stage string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("inflight publication did not reach %s", stage)
	}
}

func TestInflightPublicationCannotOverwriteNewerZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	gauge := &overlappingInflightGauge{
		Gauge:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_recompute_inflight", Help: "controlled publication overlap"}),
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		zeroWritten:  make(chan struct{}),
	}
	reg.MustRegister(gauge)
	metrics := &Metrics{recomputeInflight: gauge}
	sem := make(chan struct{}, 2)
	sem <- struct{}{}
	sem <- struct{}{}
	<-sem // The first completed worker observes one remaining recompute.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gauge.releaseFirst) }) }
	t.Cleanup(release)
	firstDone := make(chan struct{})
	go func() { metrics.setInflight(sem); close(firstDone) }()
	waitInflightPublication(t, gauge.firstEntered, "first sampled occupancy")

	<-sem // A second worker completes while the first publication is paused.
	secondStarted, secondDone := make(chan struct{}), make(chan struct{})
	go func() {
		close(secondStarted)
		metrics.setInflight(sem)
		close(secondDone)
	}()
	waitInflightPublication(t, secondStarted, "second publication attempt")
	// Without serialization the second writer publishes zero first, then the
	// paused writer overwrites it with one. With serialization it must wait.
	// This window bounds the intentionally blocked overlap, not completion time.
	select {
	case <-gauge.zeroWritten:
	case <-time.After(100 * time.Millisecond):
	}
	release()
	waitInflightPublication(t, firstDone, "first publication completion")
	waitInflightPublication(t, secondDone, "second publication completion")
	families, err := reg.Gather()
	if err != nil || len(families) != 1 || len(families[0].Metric) != 1 {
		t.Fatal("inflight metric unavailable", err)
	}
	if value := families[0].Metric[0].GetGauge().GetValue(); value != 0 {
		t.Fatalf("older publication overwrote drained occupancy: got %v, want 0", value)
	}
}
