// End-to-end proof that the break-glass tool actually moves the brake: the real
// binary path publishes over a real NATS/JetStream spine, and the real
// translate.Gate — the one webhook-ingest and the alpha Runner check — folds what
// it published and flips.
//
// The unit tests cover flag validation and payload shape, but they never call
// Publish, so they cannot catch an envelope the bus rejects at runtime (a bad
// event class, a missing tenant, a subject with no stream). Exactly the failure
// that would only surface the first time an operator reaches for the kill-switch.
//
// Gated on TEST_NATS_URL:
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test ./cmd/kanz-halt/...
package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kanz-eng/kanz/internal/platform/mode"
	"github.com/kanz-eng/kanz/internal/signal/translate"
	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestIntegration_ToolFlipsTheRealGate(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to run the halt tool against a live NATS spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// In production the PLATFORM stream ("platform.>") is provisioned by
	// infra/nats/bootstrap-job.yaml; create it here so the test is self-contained.
	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	defer setupConn.Close()
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	const streamName = "PLATFORM_HALT_IT"
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{mode.Subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), streamName) })

	// The real gate, wired to the real subject exactly as webhook-ingest wires it.
	gate := translate.OpenGate(nil)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "kanz-halt-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go func() { _ = consumer.Subscribe(subCtx, mode.Subject, "kanz-halt-it", gate.Handle) }()

	// HALT — through the tool's real entrypoint, dialing its own connection.
	haltArgs := []string{"--nats", url, "--by", "operator:akif", "--reason", "integration halt", "--tenant", "eighred"}
	if err := run(haltArgs, os.Stdout); err != nil {
		t.Fatalf("run(halt) = %v — the kill-switch could not publish", err)
	}
	waitFor(t, "gate to halt", func() bool { return gate.Halted() })

	if _, reason, _ := gate.State(); reason != "integration halt" {
		t.Fatalf("gate reason = %q, want the operator's reason to survive the wire", reason)
	}

	// RESUME — and the same gate reopens.
	resumeArgs := append(haltArgs, "--resume") //nolint:gocritic // deliberate: same flags, plus resume
	if err := run(resumeArgs, os.Stdout); err != nil {
		t.Fatalf("run(resume) = %v", err)
	}
	waitFor(t, "gate to resume", func() bool { return !gate.Halted() })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
