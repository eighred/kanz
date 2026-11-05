package main

import (
	"context"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/lake-sink/internal/config"
)

func TestSinkRefusesBrokenIdentityBeforeSubscribing(t *testing.T) {
	// No sink, readiness or telemetry is supplied: identity refusal must precede
	// all of them and must not enter the plaintext Kafka path.
	err := runSink(context.Background(), config.Config{SPIFFESocket: "invalid-socket"}, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "initialize workload identity") {
		t.Fatalf("wanted identity refusal, got %v", err)
	}
}
