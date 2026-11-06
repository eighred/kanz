package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Scoped source wiring checks. Actual SPIRE/Redpanda reads and peer refusal are
// qualified in redpanda-identity-proof; these markers do not prove a handshake.
var replayIdentityMarkers = map[string][]string{
	"pkg/bus/kafka.go": {"dialer:    KafkaReaderDialer(cfg)", "TLS:           cfg.TLSConfig", "SASLMechanism: cfg.SASL"},
	"tools/replay/reader.go": {
		"bus.KafkaReaderDialer(bus.KafkaConfig{TLSConfig: cfg.TLSConfig, SASL: cfg.SASL})",
		"r.dialer.DialContext(ctx,", "r.dialer.DialLeader(probeCtx,", "Dialer:    r.dialer,",
		"context.WithTimeout(ctx, r.cfg.PartitionDialTimeout)", "context.AfterFunc(ctx, func() { _ = conn.Close() })",
	},
	"tools/natsrebuild/topiccheck.go": {
		"bus.KafkaReaderDialer(bus.KafkaConfig{TLSConfig: c.TLSConfig, SASL: c.SASL})",
		"TLSConfig: c.TLSConfig, SASL: c.SASL", "context.WithTimeout(ctx, timeout)",
	},
	"tools/natsrebuild/cmd/nats-rebuild/main.go": {
		"kafkaIdentityRequested(os.Getenv(\"NATS_REBUILD_KAFKA_MTLS\"), socket)",
		"transport.NewMesh(ctx, socket)", "defer func() { _ = mesh.Close() }()",
		"kafkaSource.TLSConfig = mesh.Client", "Checker:   kafkaSource", "Open:      kafkaSource.Open",
	},
}

func checkReplayIdentity(files map[string]string) error {
	for name, markers := range replayIdentityMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[name], marker) {
				return fmt.Errorf("replay identity wiring missing from %s", name)
			}
		}
	}
	return nil
}

func TestReplayIdentityWiring(t *testing.T) {
	files := map[string]string{}
	for name := range replayIdentityMarkers {
		b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	if err := checkReplayIdentity(files); err != nil {
		t.Fatal(err)
	}
	for name, markers := range replayIdentityMarkers {
		for _, marker := range markers {
			original := files[name]
			files[name] = strings.ReplaceAll(original, marker, "removed-boundary")
			if checkReplayIdentity(files) == nil {
				t.Fatalf("mutation passed: %s %s", name, marker)
			}
			files[name] = original
		}
	}
}
