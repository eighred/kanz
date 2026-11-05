package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestArchiverRefusesBrokenWorkloadIdentity(t *testing.T) {
	t.Setenv("ARCHIVER_TENANT", "fixture")
	t.Setenv("ARCHIVER_NATS_URL", "nats://127.0.0.1:1")
	t.Setenv("ARCHIVER_KAFKA_BROKERS", "127.0.0.1:1")
	t.Setenv("ARCHIVER_OTLP_ENDPOINT", "")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "invalid-socket")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousOutput, previousLogger := os.Stdout, slog.Default()
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = previousOutput
		slog.SetDefault(previousLogger)
		_ = writer.Close()
		_ = reader.Close()
	})
	if code := run(); code != 2 {
		t.Fatalf("startup exit = %d, want 2", code)
	}
	_ = writer.Close()
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"msg":"spiffe source init failed"`) {
		t.Fatalf("startup did not refuse the configured identity source: %s", output)
	}
}
