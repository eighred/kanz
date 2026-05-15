package bus_test

import (
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestRetryBackoffExponentialWithCap(t *testing.T) {
	cfg := bus.RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},
		{5, 100 * time.Millisecond}, // capped (would be 160ms)
		{20, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := cfg.Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d)=%v want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestRetryBackoffZeroAndNegativeAttempt(t *testing.T) {
	cfg := bus.RetryConfig{InitialBackoff: 10 * time.Millisecond, MaxBackoff: 1 * time.Second}
	if got := cfg.Backoff(0); got != 0 {
		t.Errorf("Backoff(0)=%v want 0", got)
	}
	if got := cfg.Backoff(-1); got != 0 {
		t.Errorf("Backoff(-1)=%v want 0", got)
	}
}

func TestRetryBackoffAppliesDefaults(t *testing.T) {
	// Zero-value config still produces a sensible non-zero backoff via the
	// internal default fallback (100ms initial / 30s cap / 1 attempt).
	if got := (bus.RetryConfig{}).Backoff(1); got != 100*time.Millisecond {
		t.Errorf("zero-value Backoff(1)=%v want 100ms", got)
	}
}
