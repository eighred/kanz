package prediction

// PRED-07 — Go sync inference client. Wraps the gRPC stub for the
// inference.v1.InferenceService (PRED-06) with three resilience
// primitives:
//
//   - per-call timeout: bounded latency budget; on exceed → degraded
//     fallback (PRED-02 §2.2).
//   - circuit breaker: after N consecutive failures, fail-fast for
//     a cooldown window without hitting the wire — protects the
//     inference layer from retry storms during outages.
//   - degraded fallback: transport failures / timeouts / open
//     circuit return a DEGRADED PredictionEnvelope, NOT an error.
//     Application code branches on env.Mode, same shape as the
//     streaming path (PRED-04) — the "never black-hole" rule from
//     PRED-02 §1.
//
// # Why a separate cache from PRED-04's
//
// The streaming worker's cache (Python LastKnownCache) is a per-
// worker-process structure. PRED-07 runs in the Go orchestrator
// process — physically separate, can't share the in-memory dict.
// Two same-shape caches mirror each other; both reset on process
// restart, both seed from successful NORMAL responses on their own
// path. A future task could front them with a shared store, but for
// now per-process locality is fine — replay determinism doesn't
// require cache parity across processes.

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
)

// Degraded-reason identifiers matching PRED-02 §2 + kanz-py's
// streaming worker constants. Named bindings (not free-form
// strings at call sites) so a rename surfaces as a Go compile
// error rather than silent alert-rule breakage.
const (
	ReasonInferenceUnavailable = "inference_unavailable"
	ReasonInferenceTimeout     = "inference_timeout"
	ReasonNoCachedPrediction   = "no_cached_prediction"
	ReasonCircuitOpen          = "circuit_open"
)

// SyncClientOptions tunes the resilience primitives. Defaults via
// DefaultSyncClientOptions cover the BRAIN's <50ms target plus a
// conservative breaker.
type SyncClientOptions struct {
	// Timeout is the per-call deadline. The gRPC server treats
	// exceeded deadlines as PRED-02 §2.2 (returns DEGRADED with
	// inference_timeout); the client adds its own deadline as a
	// safety net for the case where the server doesn't honour
	// the contract.
	Timeout time.Duration
	// BreakerThreshold is the consecutive failures before the
	// circuit opens.
	BreakerThreshold int
	// BreakerCooldown is how long the circuit stays open before
	// admitting a probe call.
	BreakerCooldown time.Duration
	// Credentials are the gRPC transport credentials for the dial.
	// nil ⇒ plaintext (insecure). Set to transport.ClientCredentials
	// (SEC-01b) to speak mTLS to the inference servicer, asserting its
	// SPIFFE identity. Lives here rather than as a separate constructor
	// so the resilience options and the transport choice compose.
	Credentials credentials.TransportCredentials

	// MaxCachedSubjects caps how many subjects the degraded-fallback cache
	// holds. It is REQUIRED and has no default: a non-positive value is refused
	// at construction rather than filled in.
	//
	// WHY THIS ONE IS REFUSED WHERE THE THREE ABOVE FALL BACK (#895). Timeout,
	// BreakerThreshold and BreakerCooldown describe how THIS client behaves, and
	// this package knows enough to pick them. The subject population does not
	// belong to this package at all: Predict checks SubjectID only for
	// emptiness, and "it is a portfolio id" is a convention that lives at the
	// caller (services/risk-engine/internal/app/features.go), not a property of
	// the type. A default here would be this package inventing a bound on
	// somebody else's key space and then reading its own invention back as a
	// fact — which is the shape the cache was enumerated as an unbounded leak
	// for. Making the caller state it IS the repair.
	//
	// SIZE IT AS THE SUBJECTS EXPECTED TO BE LIVE AT ONCE, with headroom. It is
	// not a staleness policy and it is not a TTL: the cache evicts
	// least-recently-USED, every fallback read touches the entry it serves, and
	// nothing is evicted while the breaker is open. See PredictionCache.
	MaxCachedSubjects int
}

// ErrMaxCachedSubjectsRequired is returned by both constructors when
// SyncClientOptions.MaxCachedSubjects is not positive. It is a distinct error
// rather than a generic one so a composition root can say which knob it missed.
var ErrMaxCachedSubjectsRequired = errors.New(
	"prediction: SyncClientOptions.MaxCachedSubjects must be > 0 — the degraded-fallback cache is " +
		"keyed by a SubjectID this package does not constrain, so its bound is the caller's to state")

// DefaultSyncClientOptions returns conservative defaults. Tune per deployment.
//
// THE 200ms IS A CHOSEN BUDGET, NOT A DERIVED ONE, and the previous comment
// implied otherwise: it said the client should be "a touch more generous" than a
// "<50ms latency target (KANZ_BRAIN)". Both halves fail on inspection. 200ms is
// four times 50ms, not a touch. And there is NO 50ms target in this repository —
// no SLO, no alert, no dashboard, no metric; the number existed only in that
// comment, attributed to a document deleted on 2026-07-29.
//
// So the honest statement is: NOTHING MEASURES INFERENCE SERVING LATENCY. The
// nearest thing is the risk-engine/recompute-latency SLO and its
// inference-latency runbook in infra/observability/alerts/README.md, and both
// are about the CALLER —
// they fire when this breaker opens, which is a symptom of the server being slow
// rather than a measurement of it.
//
// Until something measures the server, 200ms is a bound chosen to keep a slow
// model from stalling a caller, not a figure any server was held to. Tightening
// it needs the measurement first: a timeout tuned against an imagined target
// trips on real traffic and opens the breaker, which reads as the model being
// down.
//
// IT DELIBERATELY LEAVES MaxCachedSubjects AT ZERO, so a caller that takes these
// defaults wholesale still cannot construct a client without stating its own
// subject population. That is not an oversight to be tidied up: see the field.
func DefaultSyncClientOptions() SyncClientOptions {
	return SyncClientOptions{
		Timeout:          200 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerCooldown:  10 * time.Second,
	}
}

// SyncClient is the Go client for inference.v1.InferenceService.
// Thread-safe; share one instance across goroutines.
type SyncClient struct {
	conn    *grpc.ClientConn
	stub    inferencepb.InferenceServiceClient
	cache   *PredictionCache
	breaker *CircuitBreaker
	timeout time.Duration
}

// NewSyncClient dials target via gRPC and returns a ready SyncClient.
// Transport is mTLS when opts.Credentials is set (SEC-01b), plaintext
// otherwise. Caller must Close when done.
func NewSyncClient(target string, opts SyncClientOptions) (*SyncClient, error) {
	if target == "" {
		return nil, errors.New("prediction: target is required")
	}
	creds := opts.Credentials
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	// BEFORE the dial: a client that cannot be constructed must not leave a
	// connection behind for the caller to not-Close.
	if opts.MaxCachedSubjects <= 0 {
		return nil, ErrMaxCachedSubjectsRequired
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	return newSyncClientFrom(conn, inferencepb.NewInferenceServiceClient(conn), opts)
}

// NewSyncClientWithStub constructs a SyncClient with an explicit
// stub — used by tests to swap in a fake that doesn't need a
// running gRPC server. conn is nil so Close is a no-op.
//
// It returns an error for the same reasons NewSyncClient does, so the test seam
// cannot construct a client the production constructor would refuse — a seam
// that admits a configuration the real path rejects certifies a client that
// cannot exist.
func NewSyncClientWithStub(stub inferencepb.InferenceServiceClient, opts SyncClientOptions) (*SyncClient, error) {
	return newSyncClientFrom(nil, stub, opts)
}

func newSyncClientFrom(conn *grpc.ClientConn, stub inferencepb.InferenceServiceClient, opts SyncClientOptions) (*SyncClient, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultSyncClientOptions().Timeout
	}
	if opts.BreakerThreshold <= 0 {
		opts.BreakerThreshold = DefaultSyncClientOptions().BreakerThreshold
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = DefaultSyncClientOptions().BreakerCooldown
	}
	// THE ONE OPTION WITH NO FALLBACK, checked here so both constructors get the
	// same answer from the same line — NewSyncClient repeats it only to refuse
	// before it dials.
	if opts.MaxCachedSubjects <= 0 {
		return nil, ErrMaxCachedSubjectsRequired
	}
	cache, err := NewPredictionCache(opts.MaxCachedSubjects)
	if err != nil {
		return nil, err
	}
	return &SyncClient{
		conn:    conn,
		stub:    stub,
		cache:   cache,
		breaker: NewCircuitBreaker(opts.BreakerThreshold, opts.BreakerCooldown),
		timeout: opts.Timeout,
	}, nil
}

// Close shuts down the underlying gRPC connection. No-op for
// stub-injected clients.
func (c *SyncClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Predict scores fv synchronously. ALWAYS returns a non-nil
// PredictionEnvelope — transport failures / timeouts / open
// circuit return a DEGRADED envelope rather than nil. err is
// non-nil for OBSERVABLE FAULTS (transport, validation) so
// callers can log/metric them; application logic should branch on
// env.Mode regardless of err per PRED-02 §1.
func (c *SyncClient) Predict(ctx context.Context, fv FeatureVector) (*inferencepb.PredictionEnvelope, error) {
	if fv.SubjectID == "" {
		return c.fallback(fv, ReasonInferenceUnavailable), errors.New("prediction: SubjectID required")
	}
	pbFV, err := toProtoFeatureVector(fv)
	if err != nil {
		return c.fallback(fv, ReasonInferenceUnavailable), err
	}

	if !c.breaker.Allow() {
		// Don't even attempt the call — fail fast.
		return c.fallback(fv, ReasonCircuitOpen), nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	pred, callErr := c.stub.Predict(callCtx, pbFV)
	if callErr != nil {
		c.breaker.RecordFailure()
		reason := ReasonInferenceUnavailable
		if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			reason = ReasonInferenceTimeout
		}
		return c.fallback(fv, reason), callErr
	}
	c.breaker.RecordSuccess()

	// PRED-02 §1: only NORMAL predictions seed the cache. Caching
	// a DEGRADED prediction would let it serve later requests as
	// if it were fresh — trust laundering forbidden.
	if pred != nil && pred.Mode == inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		c.cache.Store(string(fv.SubjectID), pred)
	}
	return pred, nil
}

// fallback constructs a DEGRADED PredictionEnvelope per PRED-02
// §2.1: cache hit → cached value with mode=DEGRADED + new reason;
// cache miss → zero-value with degraded_reason=no_cached_prediction.
func (c *SyncClient) fallback(fv FeatureVector, reason string) *inferencepb.PredictionEnvelope {
	cached, ok := c.cache.Lookup(string(fv.SubjectID))
	if ok {
		out := proto.Clone(cached).(*inferencepb.PredictionEnvelope)
		out.Mode = inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED
		out.DegradedReason = reason
		return out
	}
	asOf := timestamppb.Now()
	if !fv.AsOf.IsZero() {
		asOf = timestamppb.New(fv.AsOf)
	}
	return &inferencepb.PredictionEnvelope{
		SubjectId:      string(fv.SubjectID),
		Value:          0,
		Confidence:     0,
		Mode:           inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED,
		DegradedReason: ReasonNoCachedPrediction,
		AsOf:           asOf,
	}
}

// --- PredictionCache --------------------------------------------------

// PredictionCache is a per-subject_id map of the most recent NORMAL
// prediction. Mirrors PRED-04's Python LastKnownCache in shape;
// each process has its own (cross-process sharing is deliberately
// out of scope — replay determinism doesn't require it).
//
// # The bound, and why it is a cardinality rather than a TTL (#895)
//
// The map used to be documented as "overwrite-on-next-store eviction; no TTL",
// which is not eviction at all: an entry per distinct SubjectID, forever, keyed
// by a string Predict checks only for emptiness.
//
// A TTL WOULD HAVE BEEN THE WRONG SHAPE, not merely an invented number. This
// cache IS the degraded fallback — a cached prediction is what a caller gets
// while the breaker is open — so an age-based rule expires entries fastest
// during exactly the outage they exist for. It would also be this type taking
// over a staleness judgment the contract puts elsewhere: the envelope's own
// as_of is the staleness source of truth and PRED-02 §1 has the CALLER branch on
// it, which is why fallback stamps DEGRADED and a reason rather than hiding a
// stale value.
//
// So the bound is a cap on the number of subjects, and the eviction order is
// LEAST-RECENTLY-USED where "used" includes a fallback READ. Two consequences
// carry the safety argument, and both are pinned by tests:
//
//   - NOTHING IS EVICTED DURING AN OUTAGE. The map is only written by Store, and
//     Predict stores only a NORMAL response — so growth, and therefore eviction,
//     happens only on the healthy path. An open breaker cannot cost a subject
//     its cached answer.
//   - A SUBJECT STILL BEING ASKED ABOUT IS NEVER THE VICTIM. Lookup touches the
//     entry it serves, so the entries at the cold end are the subjects nobody has
//     asked about — the ones whose eviction the degraded path cannot notice.
//
// The residual, stated rather than glossed: a subject that goes quiet for longer
// than max other subjects take to cycle through, and is then asked about while
// the service is down, gets DEGRADED with no_cached_prediction instead of a
// stale value. That is a documented outcome of this contract (PRED-02 §2.1), not
// a new failure mode, and it is the one an under-sized cap trades for.
type PredictionCache struct {
	// mu is a full Mutex, not an RWMutex: Lookup WRITES now, because a read is
	// what marks a subject live. The alternative — recency updated only on Store
	// — would rank subjects by how recently the model scored them rather than by
	// how recently anyone needed them, which is the opposite of the question.
	mu      sync.Mutex
	entries map[string]cacheEntry
	// max is the subject cardinality this cache admits. Always > 0: the
	// constructor refuses anything else.
	max int
	// seq is a monotonic use counter. RECENCY IS AN ORDER, NOT A TIME, so a
	// counter answers it exactly and needs no clock, no clock injection, and no
	// tie-break between two entries touched in the same nanosecond.
	seq uint64
}

// cacheEntry is one subject's last NORMAL prediction plus the seq at which it
// was last stored or served.
type cacheEntry struct {
	pred    *inferencepb.PredictionEnvelope
	usedSeq uint64
}

// NewPredictionCache returns a cache admitting at most max subjects. max must be
// positive; see SyncClientOptions.MaxCachedSubjects for why there is no default
// to fall back to.
func NewPredictionCache(max int) (*PredictionCache, error) {
	if max <= 0 {
		return nil, ErrMaxCachedSubjectsRequired
	}
	return &PredictionCache{entries: make(map[string]cacheEntry, max), max: max}, nil
}

// Store records the latest NORMAL prediction for the subject, and drops the
// least recently used subject if that puts the cache over its cap.
func (c *PredictionCache) Store(subjectID string, pred *inferencepb.PredictionEnvelope) {
	if pred == nil || subjectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.entries[subjectID] = cacheEntry{pred: pred, usedSeq: c.seq}
	c.evictLRU()
}

// evictLRU drops least-recently-used entries until the cache is within max.
// Caller holds c.mu.
//
// ON THE WRITE PATH AND NOWHERE ELSE, for the reason orderview.Memory sweeps
// there: this map grows in exactly one place, so that is where the cap belongs,
// and a cache nobody stores into does not need a sweeper. It is also what makes
// "an open breaker evicts nothing" true by construction rather than by care —
// Predict stores only NORMAL responses, so a client that cannot reach the
// inference service never reaches this function at all.
//
// The scan is O(len) and runs only on a store that overflows the cap, so it
// costs one pass per admitted new subject once the cache is full. A heap would
// buy nothing at these sizes and would need its own invariant kept in step.
func (c *PredictionCache) evictLRU() {
	for len(c.entries) > c.max {
		var coldest string
		var coldestSeq uint64
		first := true
		for k, e := range c.entries {
			if first || e.usedSeq < coldestSeq {
				coldest, coldestSeq, first = k, e.usedSeq, false
			}
		}
		delete(c.entries, coldest)
	}
}

// Lookup returns the cached prediction and true, or nil and false. A hit COUNTS
// AS A USE: it is the fallback reading this subject's last known value, which is
// the only evidence this type has that anyone still cares about that subject.
func (c *PredictionCache) Lookup(subjectID string) (*inferencepb.PredictionEnvelope, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[subjectID]
	if !ok {
		return nil, false
	}
	c.seq++
	e.usedSeq = c.seq
	c.entries[subjectID] = e
	return e.pred, true
}

// --- CircuitBreaker ---------------------------------------------------

// CircuitBreaker is a simple consecutive-failure breaker — opens
// after N failures, stays open for cooldown, then admits one probe
// call. A successful probe closes the breaker; a failed probe
// keeps it open and resets the cooldown.
//
// Trade-off vs sony/gobreaker or hashicorp/go-resiliency: this
// implementation is ~50 lines with no dep — fine for PRED-07's
// scope. Promote to a real library if/when half-open probability,
// rolling windows, or success-ratio thresholds become useful.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	failures  int
	openedAt  time.Time
	now       func() time.Time // injected for tests
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	return &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
	}
}

// Allow reports whether a call should proceed. Closed circuit
// always allows; open circuit allows once per cooldown window.
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < b.threshold {
		return true
	}
	if b.now().Sub(b.openedAt) > b.cooldown {
		// Half-open probe: allow one call. If the probe succeeds,
		// RecordSuccess resets failures and closes the breaker.
		// If it fails, RecordFailure resets openedAt so we wait
		// another full cooldown.
		return true
	}
	return false
}

// RecordSuccess marks a successful call — resets the failure
// counter and closes the breaker.
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

// RecordFailure marks a failed call. On the Nth consecutive
// failure the breaker opens; subsequent failures during the open
// period reset the cooldown window.
func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openedAt = b.now()
	}
}

// State for tests / observability. Returns one of "closed",
// "open", "half_open".
func (b *CircuitBreaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < b.threshold {
		return "closed"
	}
	if b.now().Sub(b.openedAt) > b.cooldown {
		return "half_open"
	}
	return "open"
}
