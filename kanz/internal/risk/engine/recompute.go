package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	risk "github.com/kanz-eng/kanz/internal/risk"
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
)

// DefaultDebounceInterval coalesces the burst of state events that
// typically describes one logical portfolio change — a PortfolioState
// revalue plus its several PositionState updates arrive back-to-back, and
// recomputing once after they settle is far cheaper than once per event.
// Tunable: shorten for latency-sensitive desks, lengthen for chatty feeds.
const DefaultDebounceInterval = 250 * time.Millisecond

// Recomputer runs the engine's apply→recompute→publish reaction. On a
// Trigger it debounces per-portfolio, then takes a race-free Snapshot,
// recomputes exposure + measures, stores them in the degraded-fallback
// Cache, and emits the RISK-10 output FACTs.
//
// # Why debounce per portfolio
//
// State events for one portfolio arrive in bursts (revalue + N position
// changes). Recomputing on every apply would publish N intermediate,
// immediately-superseded exposure FACTs. A trailing-edge debounce per
// portfolio collapses the burst into a single recompute against the
// settled state — correct (the last state wins) and cheap.
//
// # Concurrency model
//
// A single worker goroutine owns the per-portfolio debounce deadlines (a
// dirty map of id→deadline). When a portfolio's deadline elapses the
// worker dispatches its recompute on its own goroutine, so different
// portfolios recompute in parallel while the bookkeeping stays in one
// place — this avoids the AfterFunc+Reset fragility (Go cannot tell a
// stopped timer from one whose func already started) that makes a
// race-free graceful drain hard. The read is a Store.Snapshot, so a
// recompute never races a concurrent apply.
type Recomputer struct {
	store     *state.Store
	registry  *compute.Registry
	cache     *risk.Cache
	publisher *publish.Publisher
	debounce  time.Duration
	logger    *slog.Logger
	metrics   *Metrics
	volModel  compute.VolModel

	// baseCtx scopes publishes + the worker; when it is canceled (hard
	// shutdown) the worker exits and in-flight recomputes skip the publish.
	baseCtx context.Context

	mu     sync.Mutex
	dirty  map[v1.PortfolioID]time.Time // id → recompute deadline (lastTrigger+debounce)
	closed bool

	wake chan struct{}  // nudges the worker to re-scan deadlines
	wg   sync.WaitGroup // tracks in-flight recompute goroutines
	done chan struct{}  // closed when the worker loop exits
}

// RecomputerOption customizes a Recomputer at construction (applied before the
// worker starts, so it is race-free).
type RecomputerOption func(*Recomputer)

// WithMetrics wires the OBS-01c RED exporter so each fired recompute records
// the kanz_risk_recompute_* series the CICD-01e canary gates on.
func WithMetrics(m *Metrics) RecomputerOption {
	return func(r *Recomputer) { r.metrics = m }
}

// WithRecomputeVolModel wires the MODEL-01g volatility model so each recompute
// enriches the snapshot's positions with a real MarketValueUncertainty band
// (RISK-08) before deriving + publishing exposure/measures. nil leaves every
// band nil. (Distinct from the query path's engine.WithVolModel — same model,
// two construction seams.)
func WithRecomputeVolModel(vm compute.VolModel) RecomputerOption {
	return func(r *Recomputer) { r.volModel = vm }
}

// NewRecomputer constructs a Recomputer and starts its worker goroutine.
// publisher may be nil (cache-only mode — useful in tests or a
// read-replica that doesn't re-emit FACTs); a non-positive debounce falls
// back to DefaultDebounceInterval. baseCtx scopes the publish side and the
// worker lifecycle. opts is variadic so existing callers are unaffected.
func NewRecomputer(
	baseCtx context.Context,
	store *state.Store,
	registry *compute.Registry,
	cache *risk.Cache,
	publisher *publish.Publisher,
	debounce time.Duration,
	logger *slog.Logger,
	opts ...RecomputerOption,
) *Recomputer {
	if debounce <= 0 {
		debounce = DefaultDebounceInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Recomputer{
		store:     store,
		registry:  registry,
		cache:     cache,
		publisher: publisher,
		debounce:  debounce,
		logger:    logger,
		baseCtx:   baseCtx,
		dirty:     make(map[v1.PortfolioID]time.Time),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	go r.loop()
	return r
}

// Trigger schedules a debounced recompute for the portfolio. Repeated
// triggers within the debounce window push the deadline out, so the burst
// collapses into one recompute fired once the window elapses with no
// further trigger. Safe for concurrent use; a no-op after Close/Drain.
func (r *Recomputer) Trigger(id v1.PortfolioID) {
	if id == "" {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.dirty[id] = time.Now().Add(r.debounce)
	r.mu.Unlock()
	r.nudge()
}

// Drain flushes every pending recompute immediately (so the last settled
// state is published) and blocks until the worker and all in-flight
// recomputes finish. baseCtx must still be live for the flushed
// recomputes to publish. After Drain the Recomputer is closed. Idempotent.
func (r *Recomputer) Drain() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		r.wg.Wait()
		return
	}
	r.closed = true
	now := time.Now()
	for id := range r.dirty { // force all deadlines due now
		r.dirty[id] = now
	}
	r.mu.Unlock()
	r.nudge()
	<-r.done
	r.wg.Wait()
}

// Close stops the worker and drops any pending recomputes WITHOUT
// flushing — the hard-stop counterpart to Drain. Blocks until the worker
// and in-flight recomputes finish. Idempotent.
func (r *Recomputer) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		r.wg.Wait()
		return
	}
	r.closed = true
	r.dirty = make(map[v1.PortfolioID]time.Time) // drop pending
	r.mu.Unlock()
	r.nudge()
	<-r.done
	r.wg.Wait()
}

// nudge wakes the worker without blocking (buffered, coalescing channel).
func (r *Recomputer) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// loop is the single worker: it scans deadlines, dispatches due recomputes
// in parallel, and sleeps until the nearest deadline or a nudge. It exits
// once closed with no remaining work, or when baseCtx is canceled.
func (r *Recomputer) loop() {
	defer close(r.done)
	for {
		r.mu.Lock()
		now := time.Now()
		var ready []v1.PortfolioID
		nextWait := time.Duration(-1)
		for id, deadline := range r.dirty {
			if !deadline.After(now) {
				ready = append(ready, id)
			} else if w := deadline.Sub(now); nextWait < 0 || w < nextWait {
				nextWait = w
			}
		}
		for _, id := range ready {
			delete(r.dirty, id)
		}
		closed, empty := r.closed, len(r.dirty) == 0
		r.mu.Unlock()

		for _, id := range ready {
			r.wg.Add(1)
			go func(id v1.PortfolioID) {
				defer r.wg.Done()
				r.recompute(id)
			}(id)
		}
		if closed && empty && len(ready) == 0 {
			return
		}
		if len(ready) > 0 {
			continue // re-scan for anything else now due
		}

		var (
			timer  *time.Timer
			timerC <-chan time.Time
		)
		if nextWait >= 0 {
			timer = time.NewTimer(nextWait)
			timerC = timer.C
		}
		select {
		case <-r.wake:
		case <-timerC:
		case <-r.baseCtx.Done():
			r.mu.Lock()
			r.closed = true
			r.dirty = make(map[v1.PortfolioID]time.Time)
			r.mu.Unlock()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// recompute snapshots the portfolio, derives exposure + measures, stores
// them in the cache, and emits the output FACTs. A store miss (portfolio
// retired between trigger and fire) is a silent no-op. Publish failures
// are logged, not retried here — the bus.Producer owns delivery
// durability, and the next apply re-triggers a recompute anyway.
func (r *Recomputer) recompute(id v1.PortfolioID) {
	p, ok := r.store.Snapshot(id)
	if !ok {
		return // portfolio retired between trigger and fire — not a recompute
	}
	start := time.Now()
	compute.PopulateUncertainty(r.baseCtx, p, r.volModel)
	es := compute.ComputeExposure(p)
	ms := compute.ComputeMeasures(p, r.registry, nil)
	r.cache.StoreExposure(id, es)
	r.cache.StoreMeasures(id, ms)

	if r.publisher == nil {
		r.metrics.observeRecompute(time.Since(start), nil)
		return
	}
	if err := r.baseCtx.Err(); err != nil {
		return // shutting down — cache is updated, skip the emit (and the metric)
	}
	var emitErr error
	if err := r.publisher.EmitExposure(r.baseCtx, es); err != nil {
		emitErr = err
		r.logger.Error("emit exposure failed", "portfolio", id, "err", err)
	}
	if err := r.publisher.EmitMeasures(r.baseCtx, ms, nil); err != nil {
		emitErr = err
		r.logger.Error("emit measures failed", "portfolio", id, "err", err)
	}
	r.metrics.observeRecompute(time.Since(start), emitErr)
}

// TriggeringApplier decorates an ingest.Applier so every successful apply
// schedules a debounced recompute for the affected portfolio. This is the
// seam that connects ORCH-01c's ingestion to the recompute reaction: the
// service wires the bus.Consumer to a TriggeringApplier wrapping the
// state.Store, so applies flow through to state AND fan out a recompute.
type TriggeringApplier struct {
	inner      ingest.Applier
	recomputer *Recomputer
}

// NewTriggeringApplier wraps inner so applies trigger r after they
// succeed. inner is normally the state.Store; r recomputes the portfolio.
func NewTriggeringApplier(inner ingest.Applier, r *Recomputer) *TriggeringApplier {
	return &TriggeringApplier{inner: inner, recomputer: r}
}

func (a *TriggeringApplier) ApplyPortfolioRevalued(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioState) error {
	if err := a.inner.ApplyPortfolioRevalued(ctx, env, p); err != nil {
		return err
	}
	a.recomputer.Trigger(v1.PortfolioID(p.PortfolioId))
	return nil
}

func (a *TriggeringApplier) ApplyPositionChanged(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PositionState) error {
	if err := a.inner.ApplyPositionChanged(ctx, env, p); err != nil {
		return err
	}
	a.recomputer.Trigger(v1.PortfolioID(p.PortfolioId))
	return nil
}

func (a *TriggeringApplier) ApplyPortfolioSnapshot(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error {
	if err := a.inner.ApplyPortfolioSnapshot(ctx, env, p); err != nil {
		return err
	}
	if p.Portfolio != nil {
		a.recomputer.Trigger(v1.PortfolioID(p.Portfolio.PortfolioId))
	}
	return nil
}

// Compile-time assertion that the decorator still satisfies the contract.
var _ ingest.Applier = (*TriggeringApplier)(nil)
