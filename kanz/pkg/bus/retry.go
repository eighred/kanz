package bus

import (
	"context"
	"strconv"
	"time"
)

// RetryConfig bounds in-handler retry. MaxAttempts is the TOTAL attempts
// including the first one; 0 or 1 means "no retry, surface or DLQ on first
// failure". InitialBackoff doubles each attempt up to MaxBackoff.
//
// Retry runs inside the Consumer's Subscribe dispatch — it blocks the
// underlying broker delivery for the duration of the retry sequence, so
// long backoffs or large MaxAttempts trade per-partition throughput for
// resilience.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

const (
	defaultMaxAttempts    = 1
	defaultInitialBackoff = 100 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second
)

func (c RetryConfig) withDefaults() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = defaultInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultMaxBackoff
	}
	return c
}

// Backoff returns the delay before the (attempt+1)-th try. `attempt` is
// 1-based — the first failure passes attempt=1 and gets InitialBackoff.
// Exponential with cap: initial, 2·initial, 4·initial, …, capped at
// MaxBackoff. No jitter at this layer; operational tuning is a follow-up.
func (c RetryConfig) Backoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	c = c.withDefaults()
	d := c.InitialBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= c.MaxBackoff || d < 0 { // <0 guards Duration overflow
			return c.MaxBackoff
		}
	}
	return d
}

func sleepWithCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// DLQ subject convention: `dlq.<original-subject>`. The NATS dlq.> stream
// (kanz/infra/nats/) and the per-topic `dlq.{name}` Kafka topics
// (kanz/infra/kafka/) are provisioned to receive these.
const dlqSubjectPrefix = "dlq."

func dlqSubject(orig string) string { return dlqSubjectPrefix + orig }

// dlqHeaders attaches failure metadata to a DLQ'd message. Original wire
// headers (incl. Nats-Msg-Id) are preserved so a DLQ consumer can still
// dedup by idempotency_key if it chooses.
func dlqHeaders(orig map[string]string, origSubject string, attempts int, err error) map[string]string {
	h := make(map[string]string, len(orig)+3)
	for k, v := range orig {
		h[k] = v
	}
	h["Kanz-DLQ-Original-Subject"] = origSubject
	h["Kanz-DLQ-Attempts"] = strconv.Itoa(attempts)
	h["Kanz-DLQ-Error"] = err.Error()
	return h
}
