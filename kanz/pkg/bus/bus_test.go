package bus_test

import (
	"context"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
)

func TestDialNATSRejectsEmptyURL(t *testing.T) {
	if _, err := bus.DialNATS(context.Background(), bus.NATSConfig{}); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestDialKafkaRejectsEmptyBrokers(t *testing.T) {
	if _, err := bus.DialKafka(bus.KafkaConfig{}); err == nil {
		t.Error("expected error for empty brokers")
	}
}
