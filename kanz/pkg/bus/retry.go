package bus

import (
	"context"
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

// The DLQ subject convention, header contract and terminal-error signal moved
// to dlq.go when #220 gave the DLQ a drain path: they are the wire contract
// between the side that PARKS a message and the side that DRAINS one, and that
// is a different concept from the in-handler retry budget this file bounds.
