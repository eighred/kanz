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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	inferencepb "github.com/kanz-eng/kanz-schemas-go/inference/v1"
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
}

// DefaultSyncClientOptions returns conservative defaults. Tune per
// deployment — the <50ms latency target (KANZ_BRAIN) is a server-
// side number; the client timeout should be a touch more generous
// (allow occasional spikes without firing the breaker).
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

// NewSyncClient dials target via gRPC (plaintext) and returns a
// ready SyncClient. Caller must Close when done.
func NewSyncClient(target string, opts SyncClientOptions) (*SyncClient, error) {
	if target == "" {
		return nil, errors.New("prediction: target is required")
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return newSyncClientFrom(conn, inferencepb.NewInferenceServiceClient(conn), opts), nil
}

// NewSyncClientWithStub constructs a SyncClient with an explicit
// stub — used by tests to swap in a fake that doesn't need a
// running gRPC server. conn is nil so Close is a no-op.
func NewSyncClientWithStub(stub inferencepb.InferenceServiceClient, opts SyncClientOptions) *SyncClient {
	return newSyncClientFrom(nil, stub, opts)
}

func newSyncClientFrom(conn *grpc.ClientConn, stub inferencepb.InferenceServiceClient, opts SyncClientOptions) *SyncClient {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultSyncClientOptions().Timeout
	}
	if opts.BreakerThreshold <= 0 {
		opts.BreakerThreshold = DefaultSyncClientOptions().BreakerThreshold
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = DefaultSyncClientOptions().BreakerCooldown
	}
	return &SyncClient{
		conn:    conn,
		stub:    stub,
		cache:   NewPredictionCache(),
		breaker: NewCircuitBreaker(opts.BreakerThreshold, opts.BreakerCooldown),
		timeout: opts.Timeout,
	}
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
type PredictionCache struct {
	mu      sync.RWMutex
	entries map[string]*inferencepb.PredictionEnvelope
}

func NewPredictionCache() *PredictionCache {
	return &PredictionCache{entries: make(map[string]*inferencepb.PredictionEnvelope)}
}

// Store records the latest NORMAL prediction for the subject.
// Overwrite-on-next-store eviction; no TTL (cached envelope's
// as_of is the staleness source of truth).
func (c *PredictionCache) Store(subjectID string, pred *inferencepb.PredictionEnvelope) {
	if pred == nil || subjectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[subjectID] = pred
}

// Lookup returns the cached prediction and true, or nil and false.
func (c *PredictionCache) Lookup(subjectID string) (*inferencepb.PredictionEnvelope, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.entries[subjectID]
	return p, ok
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
