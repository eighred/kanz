// Package lifecycle carries the one piece of shutdown bookkeeping every service
// composition root needs: remembering WHY the process is stopping, so the exit
// code can say so.
package lifecycle

import (
	"context"
	"sync"
)

// Fatal records the first terminal error a composition root hits and then stops
// the service by cancelling its context.
//
// IT EXISTS BECAUSE CANCELLING THE CONTEXT IS NOT ENOUGH. Every service in this
// estate already had sites that logged a terminal error and called the
// signal.NotifyContext stop() to bring the process down — a failed
// ListenAndServe, a consumer that died. That shutdown is byte-identical to the
// SIGTERM the kubelet sends on every rolling deploy: both cancel ctx, both
// unwind the same defers, and both used to exit 0, so the pod's own termination
// record read exitCode 0 / reason "Completed" either way and
// kube_pod_container_status_last_terminated_reason{reason="Error"} never fired.
// The benign case is the most common event in the cluster, so the failure hid
// inside it (#266). Raise keeps the reason; Code turns it into an exit status.
//
// Callers keep their own log line. This type records and stops; it does not
// report, because the log message that makes a failure diagnosable belongs at
// the site that knows what failed.
//
// ORDERING. Raise records the error BEFORE cancelling, and a composition root
// reads Code/Err only after the context is done and after every goroutine join
// that follows — so the cancellation is the happens-before edge between writer
// and reader. The mutex makes that safe rather than leaving it load-bearing on
// the reader's care, which matters because -race does not run on the usual
// development box and would not have caught getting it wrong.
type Fatal struct {
	stop context.CancelFunc

	mu  sync.Mutex
	err error
}

// NewFatal returns a Fatal that stops the service via stop when raised. Pass the
// CancelFunc from signal.NotifyContext, so a raised fatal shuts the service down
// along exactly the path a SIGTERM already takes — the difference is the exit
// code at the end of it, not the shutdown sequence.
func NewFatal(stop context.CancelFunc) *Fatal {
	return &Fatal{stop: stop}
}

// Raise records err as the reason this process is stopping and stops it. The
// FIRST error wins: a fatal cancels the context, which makes every other
// in-flight operation fail too, and those follow-on errors describe the shutdown
// rather than its cause.
//
// A nil err stops the service without marking it a failure, so a caller that
// cannot tell whether its shutdown trigger was an error does not have to
// branch — it just does not get a 1.
func (f *Fatal) Raise(err error) {
	if err != nil {
		f.mu.Lock()
		if f.err == nil {
			f.err = err
		}
		f.mu.Unlock()
	}
	f.stop()
}

// Err returns the first raised error, or nil if the shutdown was clean.
func (f *Fatal) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Code is the exit code this shutdown deserves: 1 when a fatal was raised, 0 for
// a clean one. Startup failures are not represented here — they return 2 from
// run() directly, before the service was ever ready and before anything could be
// raised.
func (f *Fatal) Code() int {
	if f.Err() != nil {
		return 1
	}
	return 0
}
