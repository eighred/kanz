package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

type arrivalReport struct {
	elapsed   time.Duration
	offered   int64
	published int64
	failed    int64
}

func (r arrivalReport) String() string {
	attempted := r.published + r.failed
	return fmt.Sprintf("elapsed=%s offered=%d attempted=%d published=%d failed=%d unsent=%d",
		r.elapsed, r.offered, attempted, r.published, r.failed, r.offered-attempted)
}

// Splitting whole seconds and the fractional second keeps the slot calculation
// exact for rates up to one slot/nanosecond, even on long runs.
func slotIndex(elapsed time.Duration, rate int64) int64 {
	return int64(elapsed/time.Second)*rate + int64(elapsed%time.Second)*rate/int64(time.Second)
}

func slotStart(index, rate int64) time.Duration {
	return time.Duration(index/rate)*time.Second +
		time.Duration((index%rate*int64(time.Second)+rate-1)/rate)
}

// Publish at most the latest due arrival. If a synchronous publish spans several
// slots, the earlier slots stay unsent. Replaying them in a burst would change
// the offered workload, and counting only calls would hide generator saturation.
func driveArrivals(parent context.Context, rate int64, duration time.Duration, publish func(context.Context) error) arrivalReport {
	start := time.Now()
	ctx, cancel := context.WithDeadline(parent, start.Add(duration))
	defer cancel()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	total := slotIndex(duration-time.Nanosecond, rate) + 1
	var report arrivalReport
	var next int64
	lastLog := start
	for {
		now := time.Now()
		report.elapsed = min(now.Sub(start), duration)
		if report.elapsed > 0 {
			report.offered = slotIndex(report.elapsed-time.Nanosecond, rate) + 1
		}
		// Include a slot attempted exactly at the current instant before a
		// cancellation (including slot zero on a sub-clock-resolution run).
		report.offered = max(report.offered, next)
		if ctx.Err() != nil || report.elapsed >= duration {
			return report
		}
		if now.Sub(lastLog) >= 10*time.Second {
			log.Printf("ingest: %s", report)
			lastLog = now
		}
		due := slotIndex(report.elapsed, rate) + 1
		if due > next {
			next = due
			if err := publish(ctx); err != nil {
				report.failed++
			} else {
				report.published++
			}
			continue
		}
		if next == total {
			<-ctx.Done()
			continue
		}
		timer.Reset(time.Until(start.Add(slotStart(next, rate))))
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}
