package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestDrainUsesConfiguredWorkloadIdentity(t *testing.T) {
	for _, socket := range []string{"", "unix:///run/spiffe/spire-agent.sock"} {
		t.Setenv("SPIFFE_ENDPOINT_SOCKET", socket)
		opt, err := parseFlags(nil)
		if err != nil {
			t.Fatal(err)
		}
		if opt.spiffeSocket != socket {
			t.Fatalf("socket = %q, want %q", opt.spiffeSocket, socket)
		}
	}
}

func TestDrainRefusesBrokenIdentityBeforeEitherBroker(t *testing.T) {
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "invalid-socket")
	err := run(context.Background(), []string{"--tenant", "fixture", "--nats-url", "nats://127.0.0.1:1", "--brokers", "127.0.0.1:1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "initialize workload identity") {
		t.Fatalf("wanted identity refusal before broker access, got %v", err)
	}
}
