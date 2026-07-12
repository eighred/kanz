package bus

import (
	"context"
	"sync"
	"time"
)

// DefaultPublishFailureThreshold is how many CONSECUTIVE publish failures mark a
// producer unhealthy. Small enough that a permanently broken publisher is caught
// in seconds; large enough that a single dropped connection does not flap a pod
// out of its Service.
const DefaultPublishFailureThreshold = 3

// publisher is the surface HealthPublisher wraps — the same one every service's
// engines already take.
type publisher interface {
	Publish(ctx context.Context, e Event) error
}

// HealthPublisher wraps a Publisher and answers one question the services could
// not previously answer: IS PUBLISHING ACTUALLY WORKING?
//
// It exists because market-ingest shipped a bug that this would have caught on
// the first deploy. It never set a tenant_id, so bus.Validate rejected EVERY
// envelope it produced. The service connected to the exchange, folded its order
// book correctly, logged a WARN per dropped snapshot — and reported /readyz 200
// the entire time. It ingested perfectly and emitted nothing, and nothing in the
// platform noticed, because readiness only ever meant "the process is up".
//
// A service whose every publish fails is NOT ready. Its FACTs are the only reason
// it exists; if none of them reach the bus, sending it traffic and calling it
// healthy is a lie that survives exactly as long as nobody looks at a downstream
// dashboard.
//
// The policy is deliberately blunt:
//
//   - Before the first attempt: HEALTHY. A pod must be allowed to come up.
//   - A success: HEALTHY, and the failure count resets. One good publish means the
//     path works.
//   - N consecutive failures: UNHEALTHY, until the next success.
//
// Consecutive, not cumulative: a transient NATS blip in the middle of a working
// day must not permanently condemn a producer that is otherwise fine. And a
// permanent failure (a malformed envelope, a missing tenant) fails every attempt
// by definition, so it trips the threshold immediately.
type HealthPublisher struct {
	inner     publisher
	threshold int

	mu          sync.RWMutex
	consecutive int
	lastErr     error
	lastSuccess time.Time
	succeeded   uint64
	failed      uint64
	now         func() time.Time
}

// NewHealthPublisher wraps inner. threshold <= 0 uses DefaultPublishFailureThreshold.
func NewHealthPublisher(inner publisher, threshold int) *HealthPublisher {
	if threshold <= 0 {
		threshold = DefaultPublishFailureThreshold
	}
	return &HealthPublisher{inner: inner, threshold: threshold, now: time.Now}
}

// Publish delegates and records the outcome. The caller's error is returned
// UNCHANGED — this wrapper observes, it never swallows. A publisher that hid the
// error to protect its own health signal would be the very bug it exists to catch.
func (h *HealthPublisher) Publish(ctx context.Context, e Event) error {
	err := h.inner.Publish(ctx, e)

	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.consecutive++
		h.failed++
		h.lastErr = err
		return err
	}
	h.consecutive = 0
	h.succeeded++
	h.lastSuccess = h.now().UTC()
	h.lastErr = nil
	return nil
}

// Healthy reports whether publishing is working: true until the threshold of
// consecutive failures is crossed, and true again after the next success.
func (h *HealthPublisher) Healthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.consecutive < h.threshold
}

// Status is Healthy plus the detail an operator needs to act: how many failures
// in a row, and the one that is doing it. A readiness endpoint that says only
// "not ready" makes someone go read logs; this lets it say WHY.
func (h *HealthPublisher) Status() (healthy bool, consecutiveFailures int, lastErr error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.consecutive < h.threshold, h.consecutive, h.lastErr
}

// Counts returns the total successes and failures observed.
func (h *HealthPublisher) Counts() (succeeded, failed uint64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.succeeded, h.failed
}

// LastSuccess is when a publish last landed (zero if none ever has).
func (h *HealthPublisher) LastSuccess() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastSuccess
}
