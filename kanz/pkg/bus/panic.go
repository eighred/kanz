package bus

import (
	"context"
	"fmt"
	"runtime/debug"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// A HANDLER PANIC MUST NOT TAKE THE PROCESS DOWN.
//
// Consumer has a full terminal-failure apparatus for handler ERRORS — bounded
// attempts, DLQ routing with Kanz-DLQ-* headers, dedup Commit/Release — and had
// none at all for handler PANICS. A nil-map write, an index out of range, a type
// assertion without the comma-ok, or a big.Rat divide-by-zero unwound straight
// past all of it and killed the pod.
//
// The cost is not one lost message. The delivery is never acked, so the broker
// redelivers it to the replacement pod, which panics on it too: one malformed
// event becomes permanent estate-wide unavailability, and the DLQ built for
// exactly this case is bypassed. That is not theoretical — a fill FACT carrying
// quantity 0 panicked the OMS position fold (#217), and #218 is why that was a
// CrashLoopBackOff rather than one parked message.
//
// A panic is converted to a TERMINAL error, never a retried one. Re-entering a
// handler that just panicked re-runs the same deterministic defect on the same
// bytes, and does it on top of whatever partial state the first attempt left —
// which is the re-entrancy hazard retryCertifiedConsumers (test/arch/bus_dlq_test.go)
// exists to prevent. There is nothing to gain by trying twice.
type HandlerPanicError struct {
	Value any
	Stack []byte
}

func (e *HandlerPanicError) Error() string {
	return fmt.Sprintf("handler panicked: %v\n%s", e.Value, e.Stack)
}

// dispatchEvent runs an EventHandler and converts a panic into a
// *HandlerPanicError. The named return is what lets the deferred recover replace
// the error the panicking call never got to produce.
func dispatchEvent(ctx context.Context, h EventHandler, env *envelopepb.Envelope, payload []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &HandlerPanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return h(ctx, env, payload)
}

// dispatchMessage is the same containment one layer lower, for the raw
// Subscribe path that does not go through Consumer.
//
// Consumer recovers before this ever sees a panic, so for a Consumer-backed
// subscription this is unreachable — it exists for direct (*NATSClient).Subscribe
// callers, of which the archiver is currently the only one
// (services/archiver/internal/archive/archiver.go). Those callers own their own
// ack contract, so the panic surfaces here as an ordinary handler error and takes
// whatever path that caller already defined for one: for the archiver, a Nak.
// A redelivery loop is a bad outcome; it is a strictly better one than the whole
// process dying, and it stays visible instead of looking like a crash.
func dispatchMessage(ctx context.Context, h Handler, msg Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &HandlerPanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return h(ctx, msg)
}
