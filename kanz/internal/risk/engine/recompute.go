package engine

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
)

// DefaultDebounceInterval coalesces the burst of state events that
// typically describes one logical portfolio change — a PortfolioState
// revalue plus its several PositionState updates arrive back-to-back, and
// recomputing once after they settle is far cheaper than once per event.
// Tunable: shorten for latency-sensitive desks, lengthen for chatty feeds.
const DefaultDebounceInterval = 250 * time.Millisecond

// DefaultEmitRetryInterval is how long after a FAILED emit a portfolio is
// re-scheduled (#620).
//
// Five seconds, and the two bounds are different in kind. Too SHORT and a broker
// outage turns every affected portfolio into a hot loop against a bus that is
// already unhealthy. Too LONG and a book that has stopped trading serves a stale
// exposure FACT as its live risk for that whole window — which is the defect
// this exists to close, so the ceiling matters more than the floor.
//
// It is not the debounce: that number collapses a burst of applies arriving
// together, and a quiet portfolio being retried has nothing to collapse with.
const DefaultEmitRetryInterval = 5 * time.Second

// DefaultRecomputeConcurrency is the dispatch ceiling when none is configured:
// how many portfolio recomputes may execute at once (#1050).
//
// GOMAXPROCS, BECAUSE THAT IS WHAT THE CONTAINER CAN ACTUALLY RUN. A recompute is
// CPU-bound work — Store.Snapshot clones the portfolio's positions, then
// PopulateUncertainty, ComputeExposure and ComputeMeasures (the whole measure set,
// VaR family included) run against the clone and stay resident until it returns.
// Above GOMAXPROCS extra goroutines do not add throughput; they multiply resident
// memory by the fan-out width and thrash the scheduler, so every recompute's
// latency degrades together and kanz_risk_recompute_duration_seconds — the ORCH-01f
// p99 the CICD-01e canary gates on - degrades with them.
//
// GOMAXPROCS IS CGROUP-AWARE IN THIS BUILD, WHICH IS THE PREMISE THIS DEFAULT
// RESTS ON. Since Go 1.25 the runtime derives the default GOMAXPROCS from the CPU
// bandwidth limit of the cgroup containing the process, for a module whose `go`
// directive is at least 1.25; kanz/go.mod declares go 1.26.1.
// risk-engine-rollout.yaml declares no resources: block, so the kanz-services
// LimitRange sizes the container at cpu: 1 — and this default therefore reads 1 in
// the pod and NumCPU on a developer box, with neither number written down
// anywhere. A future build that pins GOMAXPROCS explicitly, or a go directive
// dropped below 1.25, breaks that chain and this becomes NumCPU against a one-CPU
// limit: set RISK_ENGINE_RECOMPUTE_CONCURRENCY rather than rediscovering it as an
// OOMKill.
//
// (Verified 2026-09-05 against go.mod at 40569ca5. It is the premise, not the
// conclusion, that a future reader must re-check.)
func DefaultRecomputeConcurrency() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return 1
}

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
//
// # Why the fan-out has a ceiling (#1050)
//
// The paragraph above is why the recomputes run in PARALLEL. This one is why
// there is a limit on how parallel. The dispatch used to be one goroutine per due
// portfolio with nothing bounding it, and `ready` is every portfolio in the dirty
// map past its deadline — an estate-sized number, not a request-sized one. The
// debounce does not help: it collapses a burst of applies to ONE portfolio and
// says nothing about a burst ACROSS portfolios. So the two events that produced
// the widest fan-out were the worst two available:
//
//   - A correlated market move dirties every portfolio at once, by definition —
//     so peak width coincided exactly with the moment the numbers matter most.
//   - Every rolling deploy: Drain forces EVERY dirty portfolio due at once and
//     then blocks on wg.Wait(), against terminationGracePeriodSeconds: 60. An
//     overrun there is a SIGKILL mid-flush, which loses exactly the exposure FACTs
//     #620's reschedule logic exists to guarantee.
//
// sem is a buffered channel of DefaultRecomputeConcurrency slots, acquired by the
// worker BEFORE it dispatches. THE DISPATCHER BLOCKS AT THE CEILING; IT NEVER
// DROPS. A skipped recompute leaves that portfolio's last published exposure
// standing as its live risk, which is the failure mode #620 already ruled against
// — so the only thing a full semaphore may do is make the worker wait.
//
// IT CANNOT DEADLOCK AGAINST Drain, and the argument is short enough to check.
// The worker acquires the slot with r.mu UNLOCKED, and a slot is released by a
// recompute goroutine that needs nothing from the worker: it takes r.mu only
// briefly (markAwaitingEmit / clearAwaitingEmit) and nudge is non-blocking. So
// every acquire waits on a bounded amount of finite CPU work, Drain's <-r.done is
// reached once the dirty map empties, and wg.Wait() then joins a set already
// draining.
//
// IT DOES NOT MAKE DRAIN SLOWER IN THE SHAPE THAT MATTERS. Total drain work is
// unchanged — the same N recomputes, the same CPU — and on a one-CPU container
// the unbounded version never ran them faster, it ran them interleaved while
// holding N working sets resident. Bounded, the flush is N/ceiling batches of
// serialized CPU; unbounded it was one batch of N-way time-slicing of that same
// CPU at N times the memory. What the bound removes is the OOMKill that used to
// end the flush early.
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

	// emitRetry is how long after a FAILED emit the portfolio is re-scheduled
	// (#620). Longer than the debounce on purpose: the debounce collapses a
	// BURST of applies, while this waits out whatever made the bus unavailable,
	// and a portfolio that has stopped trading has nothing else to coalesce with.
	emitRetry time.Duration

	mu    sync.Mutex
	dirty map[v1.PortfolioID]time.Time // id → recompute deadline (lastTrigger+debounce)
	// awaiting holds portfolios whose last emit FAILED and has not since
	// succeeded — i.e. whose published exposure is not their real exposure. It
	// is the gauge's population, and it is a SET rather than a counter because
	// the question an operator asks is "is this still true", not "how often did
	// it happen".
	awaiting map[v1.PortfolioID]struct{}
	closed   bool

	// observe is the AI-M1 measure observer (nil ⇒ nothing observes). Called after the
	// risk FACTs are emitted; see WithMeasureObserver.
	observe func(context.Context, *domain.MeasureSet)

	wake chan struct{}  // nudges the worker to re-scan deadlines
	wg   sync.WaitGroup // tracks in-flight recompute goroutines
	done chan struct{}  // closed when the worker loop exits

	// sem bounds the dispatch fan-out: cap(sem) recomputes may run at once, and
	// its OCCUPANCY is what kanz_risk_recompute_inflight exports, so the gauge and
	// the ceiling it describes can never disagree. See the type doc.
	sem chan struct{}

	// undispatched is the remainder of the batch the worker is currently walking:
	// portfolios that are DUE, have already been removed from the dirty map, and
	// have not yet acquired a dispatch slot.
	//
	// IT EXISTS BECAUSE len(dirty) IS NOT THE BACKLOG, AND READING IT AS ONE
	// REPORTS ZERO IN THE ONE STATE THE GAUGE IS FOR. The worker removes the whole
	// due batch from the map in a single scan and then walks it; while it is parked
	// on the semaphore with 196 of 200 portfolios still to dispatch, the map is
	// empty. A queue-depth gauge sourced from len(dirty) alone would therefore read
	// 0 through the exact saturation an operator is watching for — an absent
	// backlog and an unreported one being the same reading, and only one of them
	// honest. (Caught by TestRecompute_WidthAndBacklogAreExported before this
	// shipped, not after.)
	undispatched atomic.Int64
}

// RecomputerOption customizes a Recomputer at construction (applied before the
// worker starts, so it is race-free).
type RecomputerOption func(*Recomputer)

// WithEmitRetryInterval overrides how long after a failed emit a portfolio is
// re-scheduled (#620). Non-positive is ignored.
//
// AN OPTION RATHER THAN A MUTABLE PACKAGE VARIABLE. A test that needs a shorter
// interval takes one here, applied at construction before the worker starts, so
// it is race-free by the same argument the RecomputerOption doc already makes.
// Relaxing DefaultEmitRetryInterval into a var "just so tests can shrink it"
// would put a data race on a value the worker goroutine reads.
func WithEmitRetryInterval(d time.Duration) RecomputerOption {
	return func(r *Recomputer) {
		if d > 0 {
			r.emitRetry = d
		}
	}
}

// WithRecomputeConcurrency overrides how many portfolio recomputes may execute at
// once (#1050). Non-positive is ignored, leaving DefaultRecomputeConcurrency.
//
// AN OPTION RATHER THAN A MUTABLE PACKAGE VARIABLE, for the reason
// WithEmitRetryInterval states above and one more: the ceiling is read by the
// WORKER goroutine on the dispatch path, so relaxing a constant into a var "just
// so a test can shrink it" would put a data race on the value that bounds the
// fan-out. Applied at construction, before the worker starts, so it is race-free.
//
// In production it is threaded from RISK_ENGINE_RECOMPUTE_CONCURRENCY by the
// composition root — the same seam a test passes through, not a second one.
func WithRecomputeConcurrency(n int) RecomputerOption {
	return func(r *Recomputer) {
		if n > 0 {
			r.sem = make(chan struct{}, n)
		}
	}
}

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

// WithMeasureObserver calls fn with each freshly recomputed MeasureSet, AFTER the risk
// FACTs have been emitted (AI-M1).
//
// It exists so the prediction layer can turn the engine's measures into features without
// the engine knowing the prediction layer exists — internal/risk imports nothing from
// internal/prediction, and the wiring lives at the composition root where it belongs.
//
// THE OBSERVER IS DOWNSTREAM OF THE EMIT, AND ITS FAILURES ARE ITS OWN. A model that
// cannot be scored must never stop the engine from computing, publishing and serving the
// risk numbers the platform actually trades on. If fn panics or blocks, that is a bug in
// fn — it is called synchronously and deliberately: an observer that fell behind silently
// would produce features that no longer describe the book they claim to.
func WithMeasureObserver(fn func(context.Context, *domain.MeasureSet)) RecomputerOption {
	return func(r *Recomputer) { r.observe = fn }
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
		emitRetry: DefaultEmitRetryInterval,
		dirty:     make(map[v1.PortfolioID]time.Time),
		awaiting:  make(map[v1.PortfolioID]struct{}),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		// The ceiling is established BEFORE the options run, so
		// WithRecomputeConcurrency overrides a real default rather than being the
		// only thing that ever creates the channel. An unset sem would be a nil
		// channel and a send on nil blocks forever — the one way a missing config
		// value could turn into a silent full stop of the whole recompute path.
		sem: make(chan struct{}, DefaultRecomputeConcurrency()),
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
	depth := len(r.dirty)
	r.mu.Unlock()
	// PUBLISHED FROM HERE AS WELL AS FROM THE WORKER, because the worker is
	// exactly where it stops being fresh: while it is parked acquiring a dispatch
	// slot for a wide batch it publishes nothing, and that is the load under which
	// an operator most needs to see the backlog moving. Both terms, for the reason
	// the undispatched field documents.
	r.metrics.setQueueDepth(depth + int(r.undispatched.Load()))
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
		// deferred is what is still IN the map: dirty, but not yet past its debounce
		// deadline. The due batch has just been removed from it, so the backlog is
		// deferred plus however much of that batch is still waiting for a slot.
		deferred := len(r.dirty)
		closed, empty := r.closed, deferred == 0
		r.mu.Unlock()
		r.undispatched.Store(int64(len(ready)))
		r.metrics.setQueueDepth(deferred + len(ready))

		for i, id := range ready {
			// ACQUIRE BEFORE DISPATCH, AND BLOCK RATHER THAN SKIP (#1050). This send
			// is the ceiling: at cap(sem) in flight the worker waits here instead of
			// starting a cap+1'th clone-plus-VaR working set. Dropping the portfolio
			// instead would leave its last published exposure standing as its live
			// risk — the exact failure #620 exists to close.
			//
			// It is also what makes the `continue` re-scan below safe. That branch
			// re-enters the loop before the batch it just dispatched has drained;
			// unbounded, each pass piled another full batch of goroutines on the
			// last. Bounded, the re-scan simply parks here until a slot frees.
			//
			// The one cost is that a baseCtx cancellation is not observed while this
			// send is parked. That is bounded by one recompute's duration and is the
			// right trade: recomputes are finite CPU work, and on a canceled baseCtx
			// each one skips its emit and returns almost immediately.
			r.sem <- struct{}{}
			r.undispatched.Store(int64(len(ready) - i - 1))
			r.metrics.setInflight(len(r.sem))
			// Depth EXCLUDES what is now in flight — that is the other gauge — so the
			// two series are disjoint and depth+inflight is the outstanding work.
			r.metrics.setQueueDepth(deferred + len(ready) - i - 1)
			r.wg.Add(1)
			go func(id v1.PortfolioID) {
				defer func() {
					<-r.sem
					r.metrics.setInflight(len(r.sem))
					r.wg.Done()
				}()
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
// retired between trigger and fire) is a silent no-op.
//
// A FAILED EMIT IS RE-SCHEDULED (#620). This paragraph used to end "publish
// failures are logged, not retried here — the bus.Producer owns delivery
// durability, and the next apply re-triggers a recompute anyway", and both
// clauses were true of a BUSY portfolio and false of a quiet one.
//
// bus.Producer owns delivery durability for a message it ACCEPTED; an
// EmitExposure that returned an error was never accepted, so there is nothing
// downstream holding it. And "the next apply re-triggers a recompute" assumes
// there is a next apply: a book that fails one emit and then stops trading has
// none, so the last successfully published exposure stood as that portfolio's
// live risk indefinitely, with every consumer treating it as current.
//
// So a failed emit now re-arms the portfolio through the same dirty map the
// debounce uses (markAwaitingEmit), and kanz_risk_portfolios_awaiting_emit says
// how many books are in that state right now — which the error COUNTER could
// not, because a counter cannot say whether the condition is still true.
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
		// SHUTTING DOWN: the cache is updated and the emit is skipped — but the
		// SKIP IS COUNTED NOW (#620). This line used to return before
		// observeRecompute, so the one signal that a recompute never reached the
		// spine was itself skipped on the path most likely to skip it. Its own
		// comment said "(and the metric)" and nothing acted on that.
		//
		// On its own series rather than a recomputeTotal status: the canary gate
		// queries ok/TOTAL with an unlabelled denominator, so a third status would
		// make every rolling deploy look like a regression. See metrics.go.
		r.metrics.observeEmitSkipped()
		return
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
	// LAST, and after the FACTs are out. The prediction layer observes the risk engine; it
	// does not gate it (AI-M1).
	if r.observe != nil {
		r.observe(r.baseCtx, ms)
	}
	// A FAILED EMIT MUST NOT BE THE LAST WORD (#620). The old argument here was
	// that "the next apply re-triggers a recompute anyway" — true of a BUSY book
	// and false of a quiet one. A portfolio that fails an emit and then stops
	// trading has no next apply, so the last successfully-published exposure
	// stands as its live risk indefinitely and every consumer treats it as
	// current. Re-scheduling is what makes the quiescent case recover.
	if emitErr != nil {
		r.markAwaitingEmit(id)
	} else {
		r.clearAwaitingEmit(id)
	}
	r.metrics.observeRecompute(time.Since(start), emitErr)
}

// markAwaitingEmit records that this portfolio's published risk is stale and
// re-arms its recompute (#620).
//
// It re-uses the SAME dirty map the debounce uses, so a retry and a genuine
// apply coalesce rather than racing: whichever deadline is nearer wins, and the
// portfolio is recomputed once. A no-op once closed, so a retry cannot resurrect
// work after Drain/Close — the worker exits on `closed && empty` and this must
// not re-populate the map behind it.
func (r *Recomputer) markAwaitingEmit(id v1.PortfolioID) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.awaiting[id] = struct{}{}
	if d, ok := r.dirty[id]; !ok || d.After(time.Now().Add(r.emitRetry)) {
		r.dirty[id] = time.Now().Add(r.emitRetry)
	}
	n := len(r.awaiting)
	r.mu.Unlock()
	r.metrics.setAwaitingEmit(n)
	r.nudge()
}

// clearAwaitingEmit records that this portfolio's published risk is current
// again. Called on every successful emit, not only after a failure, so the gauge
// cannot latch high on a portfolio that has since recovered.
func (r *Recomputer) clearAwaitingEmit(id v1.PortfolioID) {
	r.mu.Lock()
	if _, was := r.awaiting[id]; !was {
		r.mu.Unlock()
		return
	}
	delete(r.awaiting, id)
	n := len(r.awaiting)
	r.mu.Unlock()
	r.metrics.setAwaitingEmit(n)
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
