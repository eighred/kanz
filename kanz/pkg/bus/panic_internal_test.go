package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// These need no broker and no Docker, on purpose. The real-spine proof of the
// same containment is gated on TEST_NATS_URL and skips silently on a bare
// checkout — and "a handler panic does not kill the pod" must not be an invariant
// that is only checked on the machines that happen to have infrastructure up.

func TestDispatchEventConvertsAPanicIntoAnError(t *testing.T) {
	err := dispatchEvent(context.Background(),
		func(context.Context, *envelopepb.Envelope, []byte) error {
			// A nil-map write — the classic handler defect. Routed through a
			// helper so staticcheck cannot fold it away at compile time; the point
			// is that a RUNTIME panic is contained, not that one can be written.
			nilMap()["boom"] = "nil map write"
			return nil
		}, nil, nil)

	if err == nil {
		t.Fatal("a panicking handler returned nil — the panic escaped, which means it " +
			"unwound past Consumer's DLQ routing and took the process with it")
	}
	var p *HandlerPanicError
	if !errors.As(err, &p) {
		t.Fatalf("error = %T (%v), want *HandlerPanicError", err, err)
	}
	if len(p.Stack) == 0 {
		t.Error("no stack captured — a parked panic without one tells an operator that " +
			"something panicked but not where")
	}
	if !strings.Contains(p.Error(), "handler panicked") {
		t.Errorf("Error() = %q, want it to say the handler panicked", p.Error())
	}
}

func TestDispatchEventPassesAnOrdinaryErrorThrough(t *testing.T) {
	sentinel := errors.New("ordinary failure")
	err := dispatchEvent(context.Background(),
		func(context.Context, *envelopepb.Envelope, []byte) error { return sentinel },
		nil, nil)

	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the handler's own error, unwrapped", err)
	}
	var p *HandlerPanicError
	if errors.As(err, &p) {
		t.Error("an ordinary error was reported as a panic — the two take different paths " +
			"through the retry loop and have to stay distinguishable")
	}
}

func TestDispatchEventReturnsNilOnSuccess(t *testing.T) {
	if err := dispatchEvent(context.Background(),
		func(context.Context, *envelopepb.Envelope, []byte) error { return nil },
		nil, nil); err != nil {
		t.Fatalf("a succeeding handler returned %v", err)
	}
}

// The raw path, for direct (*NATSClient).Subscribe callers — today the archiver.
func TestDispatchMessageConvertsAPanicIntoAnError(t *testing.T) {
	err := dispatchMessage(context.Background(),
		func(context.Context, Message) error { panic("raw handler exploded") },
		Message{Subject: "s"})

	var p *HandlerPanicError
	if !errors.As(err, &p) {
		t.Fatalf("error = %T (%v), want *HandlerPanicError", err, err)
	}
}

// nilMap returns a nil map. Opaque to static analysis on purpose: an inline
// `var m map[string]string; m["k"] = v` is flagged as a definite nil dereference,
// and suppressing that would be suppressing a real check to keep a test.
func nilMap() map[string]string { return nil }
