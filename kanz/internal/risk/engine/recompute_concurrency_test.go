package engine_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
)

// THE ASSERTIONS BELOW ARE ABOUT OBSERVED WIDTH, NOT ABOUT CODE SHAPE (#1050).
//
// A guard that greps recompute.go for `sem <-` would pass on a semaphore built
// with a capacity of len(ready), and would fail on a correct rewrite that bounded
// the fan-out some other way. So nothing here reads the source: every claim is
// made by driving N portfolios due at the same instant and counting how many
// recomputes were actually running at the peak.
//
// THE PROBE IS INSTALLED THROUGH A SEAM PRODUCTION ALREADY THREADS.
// WithMeasureObserver is how the prediction layer observes each recompute
// (AI-M1); it is called from inside the recompute goroutine, after the FACTs are
// emitted and before the goroutine releases its dispatch slot. Concurrent
// observers are therefore concurrent recomputes, and no test-only hook — and no
// const relaxed into a var, which has already put a data race on main here — had
// to be added to the type to measure this.
//
// MEASURED ON THE UNFIXED CODE FIRST: at 64 portfolios the peak was 64, at 500 it
// was 500. The width was not "large", it was exactly N — one goroutine per due
// portfolio, which is what the dispatch literally said.

// widthProbe records the high-water mark of concurrent recomputes.
//
// It HOLDS each recompute for `hold` before returning, which is what makes the
// measurement deterministic rather than a race with the scheduler: without it a
// batch of 500 cheap recomputes can retire faster than the dispatcher starts
// them, and an unbounded fan-out would read as a narrow one.
type widthProbe struct {
	mu    sync.Mutex
	cur   int
	max   int
	calls int
	seen  map[string]int
	hold  time.Duration
}

func newWidthProbe(hold time.Duration) *widthProbe {
	return &widthProbe{seen: map[string]int{}, hold: hold}
}

func (w *widthProbe) observe(_ context.Context, ms *domain.MeasureSet) {
	w.mu.Lock()
	w.cur++
	w.calls++
	if ms != nil {
		w.seen[string(ms.PortfolioID())]++
	}
	if w.cur > w.max {
		w.max = w.cur
	}
	w.mu.Unlock()
	time.Sleep(w.hold)
	w.mu.Lock()
	w.cur--
	w.mu.Unlock()
}

func (w *widthProbe) peak() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.max
}

func (w *widthProbe) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// seedPortfolios installs n single-position portfolios and returns their ids.
func seedPortfolios(t *testing.T, s *state.Store, n int) []v1.PortfolioID {
	t.Helper()
	now := time.Now()
	ids := make([]v1.PortfolioID, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("PF-%04d", i)
		applyPosition(t, s, id, "AAPL", 1000, now)
		ids = append(ids, v1.PortfolioID(id))
	}
	return ids
}

// TestRecompute_DispatchWidthIsBoundedByTheCeiling is the issue's own "verified
// when": mark a large book dirty at once, drain it, and observe that no more than
// `ceiling` recomputes ever ran together.
//
// It fails on the pre-#1050 dispatch with a peak equal to the book size.
func TestRecompute_DispatchWidthIsBoundedByTheCeiling(t *testing.T) {
	const portfolios = 500
	const ceiling = 4

	store := state.NewStore()
	ids := seedPortfolios(t, store, portfolios)

	probe := newWidthProbe(5 * time.Millisecond)
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
		newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
		engine.WithRecomputeConcurrency(ceiling),
		engine.WithMeasureObserver(probe.observe))

	for _, id := range ids {
		r.Trigger(id)
	}
	r.Drain()

	if peak := probe.peak(); peak > ceiling {
		t.Errorf("dispatch width reached %d concurrent recomputes against a ceiling of %d "+
			"(book size %d): the fan-out is unbounded, so a correlated market move — which "+
			"dirties every portfolio at once, by definition — runs one Store.Snapshot clone "+
			"plus one full VaR working set per portfolio inside a container the LimitRange "+
			"sizes at 1 CPU / 1Gi (#1050)", peak, ceiling, portfolios)
	}

	// NON-VACUITY. An upper bound is satisfied trivially by a dispatch that
	// stopped being parallel at all, and the struct doc's parallelism argument is
	// explicitly NOT what this issue disputes. The ceiling must be reached.
	if peak := probe.peak(); peak < 2 {
		t.Errorf("dispatch width peaked at %d — the recomputes are no longer running in "+
			"parallel at all, so this test's upper bound is asserting nothing. The ceiling "+
			"was meant to cap the fan-out, not remove it", peak)
	}
}

// TestRecompute_CeilingDropsNoWork is item 2: the dispatcher BLOCKS at the
// ceiling, it does not skip. A portfolio whose recompute is skipped keeps its
// last published exposure standing as its live risk — the failure #620 closed.
func TestRecompute_CeilingDropsNoWork(t *testing.T) {
	const portfolios = 200
	const ceiling = 3

	store := state.NewStore()
	ids := seedPortfolios(t, store, portfolios)

	probe := newWidthProbe(time.Millisecond)
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
		newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
		engine.WithRecomputeConcurrency(ceiling),
		engine.WithMeasureObserver(probe.observe))

	for _, id := range ids {
		r.Trigger(id)
	}
	r.Drain() // forces every dirty portfolio due, then waits on the in-flight set

	if got := probe.total(); got != portfolios {
		t.Errorf("%d recomputes ran for %d dirty portfolios: the ceiling is DROPPING work. "+
			"A skipped recompute is not a delayed one — that portfolio's last published "+
			"exposure stands as its live risk with every consumer treating it as current "+
			"(#620)", got, portfolios)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	for _, id := range ids {
		if n := probe.seen[string(id)]; n != 1 {
			t.Errorf("portfolio %s recomputed %d times during the drain, want exactly 1", id, n)
			break
		}
	}
}

// TestRecompute_DrainTerminatesUnderTheCeiling is the deadlock question stated as
// a test: Drain forces every dirty portfolio due and then blocks on wg.Wait(),
// while the worker blocks acquiring a dispatch slot. If those two could wedge,
// every rolling deploy would hang until the 60s grace period SIGKILLed the pod
// mid-flush — strictly worse than the overrun the ceiling exists to prevent.
func TestRecompute_DrainTerminatesUnderTheCeiling(t *testing.T) {
	const portfolios = 300

	store := state.NewStore()
	ids := seedPortfolios(t, store, portfolios)

	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
		newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
		engine.WithRecomputeConcurrency(1)) // the tightest ceiling there is
	for _, id := range ids {
		r.Trigger(id)
	}

	done := make(chan struct{})
	go func() {
		r.Drain()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Drain did not return within 30s at a ceiling of 1 — the bounded dispatcher " +
			"is wedged against wg.Wait(). In production this is a rolling deploy that hangs " +
			"until terminationGracePeriodSeconds SIGKILLs the pod mid-flush, losing the " +
			"exposure FACTs the drain exists to publish (#1050)")
	}

	// Drain is idempotent and must stay so with the semaphore in the path.
	r.Drain()
}

// TestRecompute_DefaultCeilingIsWhatTheContainerCanRun pins the default. An
// unconfigured deployment is the one that runs in production today —
// risk-engine-rollout.yaml sets no RISK_ENGINE_RECOMPUTE_CONCURRENCY — so the
// default is the value that actually bounds the estate, not a fallback.
func TestRecompute_DefaultCeilingIsWhatTheContainerCanRun(t *testing.T) {
	if got, want := engine.DefaultRecomputeConcurrency(), runtime.GOMAXPROCS(0); got != want {
		t.Fatalf("DefaultRecomputeConcurrency() = %d, want GOMAXPROCS = %d", got, want)
	}

	// And it is what an unconfigured Recomputer actually uses. Constructing one
	// with no option must not leave the fan-out at book size.
	portfolios := runtime.GOMAXPROCS(0) * 40
	if portfolios < 80 {
		portfolios = 80
	}
	store := state.NewStore()
	ids := seedPortfolios(t, store, portfolios)

	probe := newWidthProbe(5 * time.Millisecond)
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
		newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
		engine.WithMeasureObserver(probe.observe))
	for _, id := range ids {
		r.Trigger(id)
	}
	r.Drain()

	if peak, want := probe.peak(), engine.DefaultRecomputeConcurrency(); peak > want {
		t.Errorf("an UNCONFIGURED Recomputer reached %d concurrent recomputes across %d "+
			"portfolios, want at most %d. The default is what production runs — a ceiling "+
			"that only exists when an operator sets an env var is not a ceiling (#1050)",
			peak, portfolios, want)
	}
}

// TestRecompute_NonPositiveCeilingKeepsTheDefault: the option must not be a way
// to remove the bound. A zero from an unset config field, or a negative from a
// typo, has to land on the default rather than on an unbounded — or, worse, a nil
// — semaphore.
func TestRecompute_NonPositiveCeilingKeepsTheDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		store := state.NewStore()
		ids := seedPortfolios(t, store, 64)
		probe := newWidthProbe(5 * time.Millisecond)
		r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
			newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
			engine.WithRecomputeConcurrency(n),
			engine.WithMeasureObserver(probe.observe))
		for _, id := range ids {
			r.Trigger(id)
		}
		done := make(chan struct{})
		go func() { r.Drain(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("WithRecomputeConcurrency(%d) wedged the dispatcher — a non-positive "+
				"ceiling must fall back to the default, never to a nil channel", n)
		}
		if peak, want := probe.peak(), engine.DefaultRecomputeConcurrency(); peak > want {
			t.Errorf("WithRecomputeConcurrency(%d): peak width %d exceeds the default ceiling %d",
				n, peak, want)
		}
		if got := probe.total(); got != 64 {
			t.Errorf("WithRecomputeConcurrency(%d): %d recomputes ran for 64 portfolios", n, got)
		}
	}
}

// gaugeValue reads one unlabelled gauge out of a registry, and distinguishes
// "registered and zero" from "never registered" — the distinction this estate has
// twice lost by registering collectors behind a branch (#973, #963), which makes
// an alert over the series silent in exactly the state it detects.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestRecompute_WidthAndBacklogAreExported covers item 3. Before this, the engine
// could not answer "how many recomputes are running", so the degradation had no
// signal at all before the OOMKill.
func TestRecompute_WidthAndBacklogAreExported(t *testing.T) {
	const portfolios = 200
	const ceiling = 4

	reg := prometheus.NewRegistry()
	metrics := engine.NewMetrics(reg)

	// REGISTERED UNCONDITIONALLY. NewMetrics is called from the composition root
	// before any broker, database or shard decision is made, and both series must
	// exist on a bare engine that has done no work — an absent series and a zero
	// are the same reading to a rule, and only one of them is honest.
	if v, ok := gaugeValue(t, reg, "kanz_risk_recompute_inflight"); !ok || v != 0 {
		t.Fatalf("kanz_risk_recompute_inflight before any work: value=%v present=%v — want "+
			"present and 0. A gauge registered behind a branch exports NO series on the "+
			"deployment that took the other branch, and an alert over it is silent in "+
			"exactly the state it was written for (#973, #963)", v, ok)
	}
	if v, ok := gaugeValue(t, reg, "kanz_risk_recompute_queue_depth"); !ok || v != 0 {
		t.Fatalf("kanz_risk_recompute_queue_depth before any work: value=%v present=%v — "+
			"want present and 0", v, ok)
	}

	store := state.NewStore()
	ids := seedPortfolios(t, store, portfolios)

	var (
		mu       sync.Mutex
		peakGaug float64
		peakQueu float64
	)
	sample := func(context.Context, *domain.MeasureSet) {
		v, ok := gaugeValue(t, reg, "kanz_risk_recompute_inflight")
		if !ok {
			t.Error("kanz_risk_recompute_inflight vanished mid-run")
			return
		}
		q, _ := gaugeValue(t, reg, "kanz_risk_recompute_queue_depth")
		mu.Lock()
		if v > peakGaug {
			peakGaug = v
		}
		if q > peakQueu {
			peakQueu = q
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}

	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(),
		newPublisher(t, newEmitCounter()), time.Millisecond, discard(),
		engine.WithMetrics(metrics),
		engine.WithRecomputeConcurrency(ceiling),
		engine.WithMeasureObserver(sample))
	for _, id := range ids {
		r.Trigger(id)
	}
	r.Drain()

	mu.Lock()
	gotInflight, gotQueue := peakGaug, peakQueu
	mu.Unlock()

	if gotInflight < 1 {
		t.Errorf("kanz_risk_recompute_inflight never rose above %v while %d recomputes ran — "+
			"the gauge is registered and dead, which reads to an operator exactly like an "+
			"idle engine", gotInflight, portfolios)
	}
	if gotInflight > ceiling {
		t.Errorf("kanz_risk_recompute_inflight peaked at %v against a ceiling of %d — the "+
			"gauge and the bound it describes disagree", gotInflight, ceiling)
	}
	if gotQueue < 1 {
		t.Errorf("kanz_risk_recompute_queue_depth never rose above %v while %d portfolios "+
			"were dirty — the backlog behind the ceiling is the half of the pair that says "+
			"whether saturation is healthy or a queue the container cannot work off",
			gotQueue, portfolios)
	}

	// After the drain the width must return to zero: a gauge that latches high is
	// a permanent page.
	if v, ok := gaugeValue(t, reg, "kanz_risk_recompute_inflight"); !ok || v != 0 {
		t.Errorf("kanz_risk_recompute_inflight after Drain: value=%v present=%v — want 0. "+
			"A width gauge that does not return to zero pages forever", v, ok)
	}
	if v, ok := gaugeValue(t, reg, "kanz_risk_recompute_queue_depth"); !ok || v != 0 {
		t.Errorf("kanz_risk_recompute_queue_depth after Drain: value=%v present=%v — want 0", v, ok)
	}
}
