package bus

// These tests pin the fix for a silent-failure bug: NATSClient's JetStream
// Consume callbacks used to discard the error from both Msg.Ack() and
// Msg.Nak() outright. A failed Ack is the serious case — the handler already
// did the work, but the broker is never told, so it redelivers the same
// message forever while the service reports healthy. See logAckFailure and
// logNakFailure in nats.go.
//
// jetstream.Msg is exercised through cons.Consume(), which is only reachable
// against a live NATS/JetStream server (see nats_integration_test.go, which
// skips without TEST_NATS_URL). There is no fake jetstream.Msg anywhere in
// this package to construct one without a broker. So this test exercises the
// extracted logging helpers directly — the honest substitute the task
// description itself calls out — rather than building an elaborate fake
// broker harness to reach two lines inside a real Consume callback.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureLogger returns an slog.Logger writing structured text to buf, so
// assertions can check both the message and the individual attributes
// (subject, group, err) rather than one opaque blob.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestLogAckFailureReportsConsequenceNotJustFact(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	wantErr := errors.New("permission violation: publish to JS.ACK.abc")

	logAckFailure(logger, "order.order.submit", "oms", wantErr)

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected ERROR level, got: %s", out)
	}
	if !strings.Contains(out, "subject=order.order.submit") {
		t.Errorf("expected subject in log, got: %s", out)
	}
	if !strings.Contains(out, "group=oms") {
		t.Errorf("expected group in log, got: %s", out)
	}
	if !strings.Contains(out, wantErr.Error()) {
		t.Errorf("expected underlying error text in log, got: %s", out)
	}
	// The message must explain the CONSEQUENCE (redelivered forever, looks
	// healthy) and the likely cause ($JS.ACK.>), not just restate "ack failed".
	for _, want := range []string{"redeliver", "healthy", "$JS.ACK.>"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected log message to mention %q (consequence/cause), got: %s", want, out)
		}
	}
}

func TestLogAckFailureDefaultsNilLoggerWithoutPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("logAckFailure panicked with nil logger: %v", r)
		}
	}()
	logAckFailure(nil, "some.subject", "some-group", errors.New("boom"))
}

func TestLogNakFailureReportsSubjectGroupAndErr(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	wantErr := errors.New("connection closed")

	logNakFailure(logger, "order.order.amend", "oms", wantErr)

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected ERROR level, got: %s", out)
	}
	if !strings.Contains(out, "subject=order.order.amend") {
		t.Errorf("expected subject in log, got: %s", out)
	}
	if !strings.Contains(out, "group=oms") {
		t.Errorf("expected group in log, got: %s", out)
	}
	if !strings.Contains(out, wantErr.Error()) {
		t.Errorf("expected underlying error text in log, got: %s", out)
	}
}

func TestLogNakFailureDefaultsNilLoggerWithoutPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("logNakFailure panicked with nil logger: %v", r)
		}
	}()
	logNakFailure(nil, "some.subject", "", errors.New("boom"))
}
