package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestSlowPublisherAccountsForEveryOfferedArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts []time.Duration
		start := time.Now()
		got := driveArrivals(t.Context(), 10, time.Second, func(context.Context) error {
			attempts = append(attempts, time.Since(start))
			time.Sleep(250 * time.Millisecond)
			if len(attempts) == 2 {
				return errors.New("unacknowledged publish")
			}
			return nil
		})
		if !slices.Equal(attempts, []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, 750 * time.Millisecond}) ||
			got.String() != "elapsed=1s offered=10 attempted=4 published=3 failed=1 unsent=6" {
			t.Fatalf("attempts=%v report=%s", attempts, got)
		}
	})
}

func TestArrivalScheduleUsesExactRateAndStopsAtCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(920*time.Millisecond, cancel)
		start := time.Now()
		var attempts []time.Duration
		got := driveArrivals(ctx, 3, time.Minute, func(context.Context) error {
			attempts = append(attempts, time.Since(start))
			return nil
		})
		if !slices.Equal(attempts, []time.Duration{0, 333333334, 666666667}) ||
			got.String() != "elapsed=920ms offered=3 attempted=3 published=3 failed=0 unsent=0" {
			t.Fatalf("attempts=%v report=%s", attempts, got)
		}
	})
}
