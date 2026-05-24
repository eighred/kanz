package bus

// Broker-free unit tests for the SEC-01c TLS/SASL wiring on the Kafka client.
// The live mTLS-handshake / plaintext-rejection tests are SEC-01e + the gated
// kafka_integration_test; these assert the config actually reaches the writer
// Transport and the reader Dialer (the easy-to-silently-drop plumbing).

import (
	"crypto/tls"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
)

func TestDialKafka_TLSAndSASLReachWriterTransport(t *testing.T) {
	tlsCfg := &tls.Config{ServerName: "kafka"}
	mech := plain.Mechanism{Username: "u", Password: "p"}
	c, err := DialKafka(KafkaConfig{Brokers: []string{"b:9093"}, TLSConfig: tlsCfg, SASL: mech})
	if err != nil {
		t.Fatalf("DialKafka: %v", err)
	}
	defer c.Close()

	tr, ok := c.writer.Transport.(*kafka.Transport)
	if !ok {
		t.Fatalf("writer.Transport = %T want *kafka.Transport", c.writer.Transport)
	}
	if tr.TLS != tlsCfg {
		t.Error("writer Transport.TLS not the configured *tls.Config")
	}
	if tr.SASL == nil {
		t.Error("writer Transport.SASL not set")
	}
	if c.dialer == nil || c.dialer.TLS != tlsCfg || c.dialer.SASLMechanism == nil {
		t.Errorf("reader dialer missing TLS/SASL: %+v", c.dialer)
	}
}

// Neither TLS nor SASL ⇒ no custom reader dialer (kafka-go DefaultDialer),
// preserving the prior plaintext behaviour exactly.
func TestDialKafka_PlaintextHasNilDialer(t *testing.T) {
	c, err := DialKafka(KafkaConfig{Brokers: []string{"b:9092"}})
	if err != nil {
		t.Fatalf("DialKafka: %v", err)
	}
	defer c.Close()
	if c.dialer != nil {
		t.Errorf("plaintext dialer = %+v want nil", c.dialer)
	}
}
