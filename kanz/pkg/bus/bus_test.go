package bus_test

import (
	"context"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
)

func TestDialNATSRejectsEmptyURL(t *testing.T) {
	if _, err := bus.DialNATS(context.Background(), bus.NATSConfig{}); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestDialNATSCredentialsFileRefusesMixedAuthentication(t *testing.T) {
	for _, cfg := range []bus.NATSConfig{
		{URL: "localhost:4222", Token: "fixture"},
		{URL: "localhost:4222", Username: "fixture"},
		{URL: "localhost:4222", Password: "fixture"},
		{URL: "localhost:4222, nats://fixture@localhost:4223"},
		{URL: "fixture@localhost:4222"},
	} {
		cfg.CredentialsFile = "missing.creds"
		if _, err := bus.DialNATS(context.Background(), cfg); err == nil ||
			!strings.HasPrefix(err.Error(), "nats: credentials file") ||
			strings.Contains(err.Error(), "fixture") {
			t.Fatalf("expected a credential-free configuration refusal, got %v", err)
		}
	}
}

func TestDialKafkaRejectsEmptyBrokers(t *testing.T) {
	if _, err := bus.DialKafka(bus.KafkaConfig{}); err == nil {
		t.Error("expected error for empty brokers")
	}
}
