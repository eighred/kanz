package bus

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// DLQ subject convention: `dlq.<original-subject>`. The NATS dlq.> stream
// (kanz/infra/nats/) and the per-topic `dlq.{name}` Kafka topics
// (kanz/infra/kafka/) are provisioned to receive these.
const dlqSubjectPrefix = "dlq."

func dlqSubject(orig string) string { return dlqSubjectPrefix + orig }

// IsDLQSubject reports whether subject sits in the reserved DLQ namespace
// (kanz-schemas/docs/subject-taxonomy.md §6).
//
// Exported because the DRAIN side has to ask the question in both directions:
// the subject it reads FROM must be a DLQ subject, and the subject it
// republishes TO must not be — that second check is what stops a redrive from
// building `dlq.dlq.order.order.submit` out of a corrupt header and burying a
// capital-path order one level deeper.
func IsDLQSubject(subject string) bool { return strings.HasPrefix(subject, dlqSubjectPrefix) }

// THE DLQ WIRE CONTRACT.
//
// These header names are the ONLY link between the side that parks a message
// (Consumer.publishDLQ) and the side that drains one (Redriver,
// cmd/kanz-redrive). Until #220 there was no drain side at all, so the names
// were free to drift: they were string literals at their single use site, and
// services/archiver independently grew its own near-miss spelling of the same
// idea — `Kanz-DLQ-Subject` where this package writes
// `Kanz-DLQ-Original-Subject`. Nothing caught it because nothing read either
// one. That is the `secret()` shape from CLAUDE.md, caught one consumer earlier
// than usual.
//
// They are constants here so the parker and the drainer cannot disagree about
// them without a compile error. A drain path keyed on a header the parker
// stopped writing does not fail loudly — it finds nothing, reports "0 messages
// redriven", and reads exactly like a DLQ that is legitimately empty.
const (
	// HeaderDLQOriginalSubject is the subject the message was published to
	// before it failed — the address a redrive sends it back to. Without it a
	// parked message is unroutable and the redrive REFUSES rather than guessing.
	HeaderDLQOriginalSubject = "Kanz-DLQ-Original-Subject"
	// HeaderDLQAttempts is how many times the handler actually ran (0 when the
	// failure was pre-dispatch: unframe or envelope validation).
	HeaderDLQAttempts = "Kanz-DLQ-Attempts"
	// HeaderDLQError is the error string that ended the delivery.
	HeaderDLQError = "Kanz-DLQ-Error"
	// HeaderDLQParkedAt is when the message was parked, RFC3339Nano UTC.
	//
	// It exists because the redrive has to know a message's AGE, and neither the
	// envelope's event_time (when the business event happened, possibly days
	// earlier) nor the JetStream message timestamp (not carried through
	// bus.Message, and absent entirely on the Kafka DLQ topics) answers that
	// question. See RedriveOptions.MinAge for what the age is used for.
	HeaderDLQParkedAt = "Kanz-DLQ-Parked-At"
	// HeaderDLQClass is ClassTransient or ClassTerminal — see IsTerminal.
	HeaderDLQClass = "Kanz-DLQ-Class"
	// HeaderDLQRedrives is how many times this message has ALREADY been redriven
	// and parked again. It is the loop bound, and it works because dlqHeaders
	// copies the inbound wire headers verbatim: a redriven message carries the
	// count forward, so when it fails a second time the count rides back into the
	// DLQ with it and the next redrive sees a higher number.
	HeaderDLQRedrives = "Kanz-DLQ-Redrives"
)

// Failure classes recorded in HeaderDLQClass. They do NOT change what the
// Consumer does — both park immediately on the first failure, which is the
// deliberate, arch-guarded behaviour (test/arch/bus_dlq_test.go). They change
// what the DRAIN does with the parked message, which is the only place the
// distinction can be acted on safely.
const (
	// ClassTransient: the failure was about the WORLD, not the message. A
	// Postgres failover, a NATS reconnect, a venue gate timeout. The same bytes
	// through the same handler will very likely succeed later, so a redrive is
	// the right response.
	ClassTransient = "transient"
	// ClassTerminal: the failure was about the MESSAGE. Unframeable bytes, an
	// envelope that fails validation, a handler panic. Redriving it re-runs a
	// deterministic defect against the same bytes and parks it again — that is
	// the loop, and a redrive skips these unless an operator asks for them
	// explicitly.
	ClassTerminal = "terminal"
)

// ErrTerminal is the sentinel a handler wraps into its error to say "these
// BYTES are the problem; do not send them back to me". Match with IsTerminal,
// never by string.
var ErrTerminal = errors.New("terminal failure")

// Terminal marks err as terminal — the message itself is unprocessable and
// redriving it would only reproduce the same failure.
//
// The returned error's Error() is UNCHANGED, deliberately: HeaderDLQError is
// read by a human during an incident and prefixing every one of them with
// "terminal failure: " would push the actual cause further from the front of
// the line. The classification travels in HeaderDLQClass, which is a field, not
// a prose prefix.
//
// Terminal(nil) returns nil, so a handler can `return bus.Terminal(f())`
// without a nil check changing its success path into a failure.
func Terminal(err error) error {
	if err == nil {
		return nil
	}
	return &terminalError{err: err}
}

// IsTerminal reports whether err is a failure that a redrive should not repeat.
//
// A handler PANIC counts without any handler cooperation: consumer.go already
// treats a panic as terminal for retry purposes ("re-entering a handler that
// just panicked re-runs the same deterministic defect on the same bytes"), and
// the identical argument decides the redrive question.
//
// THE DEFAULT IS TRANSIENT, AND THE ASYMMETRY IS THE REASON. A handler that
// says nothing gets ClassTransient. The two costs are not equal, but which one
// is worse INVERTED when #220 gave the DLQ a drain:
//
//   - Before a drain existed, a wrong guess in either direction was
//     unrecoverable, so there was no safe default to pick.
//   - With a drain, a wrongly-transient classification costs at most
//     RedriveOptions.MaxRedrives wasted attempts, bounded and visible in
//     HeaderDLQRedrives. A wrongly-terminal one hides a live capital-path order
//     behind an extra operator flag during an incident.
//
// So the default is the one whose failure mode is "some wasted work, loudly
// counted" rather than "a real order that needs a second decision to find". The
// cases that are PROVABLY terminal — unframe, envelope validation, panic — are
// classified as such by this package without relying on a handler to remember,
// which is what keeps the default from being a way to smuggle poison messages
// into the redrive path.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTerminal) {
		return true
	}
	var panicked *HandlerPanicError
	return errors.As(err, &panicked)
}

type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

// Is answers errors.Is(err, ErrTerminal) without ErrTerminal being anywhere in
// the wrapped chain — the marker is the TYPE, and the wrapped error keeps its
// own identity for every other errors.Is/As the caller might do.
func (e *terminalError) Is(target error) bool { return target == ErrTerminal }

// classOf maps a dispatch failure to the HeaderDLQClass value recorded with it.
func classOf(err error) string {
	if IsTerminal(err) {
		return ClassTerminal
	}
	return ClassTransient
}

// dlqHeaders attaches failure metadata to a DLQ'd message. Original wire
// headers (incl. Nats-Msg-Id, and HeaderDLQRedrives on a message that has been
// through a redrive before) are preserved so a DLQ consumer can still dedup by
// idempotency_key if it chooses — and so the redrive loop bound survives the
// round trip.
func dlqHeaders(orig map[string]string, origSubject string, attempts int, err error, parkedAt time.Time) map[string]string {
	h := make(map[string]string, len(orig)+5)
	for k, v := range orig {
		h[k] = v
	}
	h[HeaderDLQOriginalSubject] = origSubject
	h[HeaderDLQAttempts] = strconv.Itoa(attempts)
	h[HeaderDLQError] = err.Error()
	h[HeaderDLQParkedAt] = parkedAt.UTC().Format(time.RFC3339Nano)
	h[HeaderDLQClass] = classOf(err)
	return h
}
