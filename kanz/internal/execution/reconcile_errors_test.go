package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestReconcileErrorObserverSeedClassifyBoundAndRedact(t *testing.T) {
	reg := prometheus.NewRegistry()
	var log bytes.Buffer
	o := NewReconcileErrorObserver(reg, slog.New(slog.NewJSONHandler(&log, nil)), "BINANCE")
	now := time.Unix(1700000000, 0)
	o.now = func() time.Time { return now }
	families, err := reg.Gather()
	if err != nil || len(families) != 1 || len(families[0].Metric) != 15 {
		t.Fatal("missing zero-seeded bounded series", err)
	}
	for _, tc := range []struct {
		err    error
		reason string
	}{{ErrRateLimited, "rate_limited"}, {ErrReconcileEvidence, "invalid_evidence"}, {ErrEgressDenied, "access_denied"}, {context.DeadlineExceeded, "timeout"}, {errors.New("PRIVATE response"), "other"}} {
		for i := 0; i < 10; i++ {
			o.Observe(t.Context(), "reconcile", fmt.Errorf("PRIVATE URL: %w", tc.err))
		}
		if got := testutil.ToFloat64(o.counter.WithLabelValues("reconcile", tc.reason)); got != 10 {
			t.Fatalf("%s count %v", tc.reason, got)
		}
	}
	if strings.Contains(log.String(), "PRIVATE") || strings.Count(log.String(), "\n") != 5 {
		t.Fatal("unbounded or sensitive diagnostics", log.String())
	}
	now = now.Add(time.Minute)
	o.Observe(t.Context(), "reconcile", ErrRateLimited)
	if strings.Count(log.String(), "\n") != 6 {
		t.Fatal("log did not resume after bound")
	}
	o.Observe(t.Context(), "untrusted-loop", errors.New("untrusted-error"))
	if testutil.ToFloat64(o.counter.WithLabelValues("unknown", "other")) != 1 {
		t.Fatal("unbounded loop labels")
	}
}

func TestReconcileErrorCancellationAndConcurrentLoops(t *testing.T) {
	reg := prometheus.NewRegistry()
	o := NewReconcileErrorObserver(reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "OKX")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	o.Observe(ctx, "reconcile", fmt.Errorf("shutdown: %w", context.Canceled))
	o.Observe(ctx, "healing", nil)
	// A failed child deadline while the parent is live is still an incident.
	o.Observe(t.Context(), "reconcile", context.DeadlineExceeded)
	if testutil.ToFloat64(o.counter.WithLabelValues("reconcile", "other")) != 0 || testutil.ToFloat64(o.counter.WithLabelValues("reconcile", "timeout")) != 1 {
		t.Fatal("cancellation confused with failed pass")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); o.Observe(t.Context(), "healing", ErrReconcileEvidence) }()
	}
	wg.Wait()
	if testutil.ToFloat64(o.counter.WithLabelValues("healing", "invalid_evidence")) != 100 {
		t.Fatal("concurrent errors lost")
	}
}
