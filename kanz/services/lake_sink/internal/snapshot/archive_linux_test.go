//go:build linux && amd64

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/segmentio/kafka-go"
)

func startActualSnapshotArchiver(t *testing.T, ctx context.Context, b *snapshotBroker, broker, tenant string) *snapshotOwnerProcess {
	t.Helper()
	path := os.Getenv("TEST_LAKE_ARCHIVER_TEST_BIN")
	if !filepath.IsAbs(path) {
		t.Fatal("actual archiver executable required")
	}
	leaf, err := b.archiver.GetX509SVID()
	if err != nil {
		t.Fatal("archiver identity unavailable")
	}
	cert, key, err := leaf.Marshal()
	if err != nil {
		t.Fatal("archiver identity encoding failed")
	}
	bundle, err := b.archiver.GetX509BundleForTrustDomain(leaf.ID.TrustDomain())
	if err != nil {
		t.Fatal("archiver trust unavailable")
	}
	trust, err := bundle.Marshal()
	if err != nil {
		t.Fatal("archiver trust encoding failed")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("archiver fixture requires bounded parent")
	}
	in := struct {
		Deadline                                      time.Time
		NATSURL, KafkaBroker, Tenant, ExpectedAccount string
		Cert, Key, Bundle                             []byte
		ArchiverID, BrokerID                          string
	}{deadline, "tls://" + b.address, broker, tenant, "acme", cert, key, trust, leaf.ID.String(), "spiffe://kanz.internal/ns/kanz-messaging/sa/nats"}
	wire, err := json.Marshal(in)
	if err != nil {
		t.Fatal("archiver input encoding failed")
	}
	p := startSnapshotFixtureProcess(t, ctx, path, "TestLakeSnapshotArchiverSubprocess", "KANZ_LAKE_ARCHIVER_CHILD=1", wire)
	select {
	case state := <-p.states:
		if state != "connected" {
			t.Fatal("actual archiver did not register")
		}
	case <-ctx.Done():
		t.Fatal("actual archiver registration deadline")
	}
	return p
}

// Real native subscription -> existing Archiver.Handle -> actual Kafka ACK.
// This is a disposable loopback transport proof, not production Kafka ACL,
// SPIRE deployment, historical completeness or DR acceptance.
func TestSnapshotActualCanonicalArchiverFourFactsRestartAndTenantRefusal(t *testing.T) {
	broker := os.Getenv("TEST_LAKE_KAFKA_BROKER")
	if broker == "" {
		t.Skip("dedicated actual Kafka fixture required")
	}
	host, port, err := net.SplitHostPort(broker)
	if err != nil || port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("Kafka fixture requires literal loopback")
	}
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCrashFixtureLifetime)
	defer cancel()
	tenant := "lake-fixture-" + uuid.NewString()
	topic := tenant + ".lake.snapshot"
	client := &kafka.Client{Addr: kafka.TCP(broker), Timeout: 5 * time.Second}
	owned := []string{topic, "dlq.archiver"}
	metadata, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: owned})
	if err != nil || len(metadata.Topics) != 2 {
		t.Fatal("Kafka inventory unavailable")
	}
	for _, entry := range metadata.Topics {
		if !errors.Is(entry.Error, kafka.UnknownTopicOrPartition) {
			t.Fatal("fixture topic already exists or absence unknown")
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		deleted, e := client.DeleteTopics(cleanup, &kafka.DeleteTopicsRequest{Topics: owned})
		if e != nil || len(deleted.Errors) != 2 || (deleted.Errors[topic] != nil && !errors.Is(deleted.Errors[topic], kafka.UnknownTopicOrPartition)) || (deleted.Errors["dlq.archiver"] != nil && !errors.Is(deleted.Errors["dlq.archiver"], kafka.UnknownTopicOrPartition)) {
			t.Error("owned Kafka topic removal failed")
			return
		}
		for {
			md, e := client.Metadata(cleanup, &kafka.MetadataRequest{Topics: owned})
			absent := e == nil && len(md.Topics) == 2
			if absent {
				for _, entry := range md.Topics {
					absent = absent && errors.Is(entry.Error, kafka.UnknownTopicOrPartition)
				}
			}
			if absent {
				return
			}
			select {
			case <-cleanup.Done():
				t.Error("owned Kafka topic absence unconfirmed")
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	created, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}, {Topic: "dlq.archiver", NumPartitions: 1, ReplicationFactor: 1}}})
	if err != nil || len(created.Errors) != 2 || created.Errors[topic] != nil || created.Errors["dlq.archiver"] != nil {
		t.Fatal("owned Kafka topic provisioning failed")
	}

	for {
		md, e := client.Metadata(ctx, &kafka.MetadataRequest{Topics: owned})
		ready := e == nil && len(md.Topics) == 2
		if ready {
			for _, entry := range md.Topics {
				ready = ready && entry.Error == nil && len(entry.Partitions) == 1 && entry.Partitions[0].Leader.ID >= 0
			}
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Kafka leaders unavailable")
		case <-time.After(20 * time.Millisecond):
		}
	}
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("broker admin unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("JetStream unavailable")
	}
	if _, err = js.CreateStream(ctx, snapshotprofile.Stream()); err != nil {
		t.Fatal("snapshot stream unavailable")
	}
	archiver := startActualSnapshotArchiver(t, ctx, b, broker, tenant)
	owner := b.client(ctx, func(error) {})
	if owner.RequireAccountIdentity(ctx, "acme", "spiffe://kanz.internal/ns/kanz-services/sa/lake-sink") != nil || owner.RequireRegisteredDefaultDurable(ctx, snapshotprofile.Name, "lake.snapshot.>", "archiver") != nil {
		t.Fatal("actual same-account archiver registration unproved")
	}
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: topic, Partition: 0, MinBytes: 1, MaxBytes: 1 << 20})
	defer func() {
		if reader.Close() != nil {
			t.Error("Kafka reader close failed")
		}
	}()
	dlq := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: "dlq.archiver", Partition: 0, MinBytes: 1, MaxBytes: 1 << 20})
	defer func() {
		if dlq.Close() != nil {
			t.Error("Kafka parking reader close failed")
		}
	}()
	publish := func(state lakepb.SnapshotState, scope string) []byte {
		t.Helper()
		// Canonical Producer owns FACT identity/stamping. The capture is only an
		// encoder; broker ACK and archival bytes are verified independently below.
		capture := &lifecycleCapture{}
		producer, e := bus.NewProducer(capture, bus.ProducerConfig{Source: Service, ProducerVersion: "fixture", Tenant: scope})
		if e != nil {
			t.Fatal("canonical fixture producer unavailable")
		}
		record, e := lifecycleRecord(ctx, scope, &lakepb.Snapshot{SnapshotId: "fixture-snapshot", State: state, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}, bytes.Repeat([]byte{1}, 32), 0, time.Now())
		if e != nil {
			t.Fatal("canonical fixture lifecycle unavailable")
		}
		event, e := record.Event()
		if e != nil || producer.Publish(ctx, event) != nil || len(capture.messages) != 1 {
			t.Fatal("canonical fixture framing refused")
		}
		subject := capture.messages[0].Subject
		wire := bytes.Clone(capture.messages[0].Body)
		// Extra outer unknown field and noncanonical varint must survive verbatim.
		wire = append(wire, 0xa0, 0x06, 0x81, 0x00)
		if _, e = js.Publish(ctx, subject, wire); e != nil {
			t.Fatal("actual canonical broker publication failed")
		}
		return wire
	}
	read := func(r *kafka.Reader, want []byte, offset int64, parked bool) {
		t.Helper()
		msg, e := r.FetchMessage(ctx)
		if e != nil || !bytes.Equal(msg.Value, want) || msg.Partition != 0 || msg.Offset != offset {
			t.Fatal("actual archival bytes/offset differ")
		}
		env, _, e := bus.Unframe(want)
		if e != nil {
			t.Fatal("expected canonical frame invalid")
		}
		headers := map[string]string{}
		for _, h := range msg.Headers {
			headers[h.Key] = string(h.Value)
		}
		if headers["Kanz-Event-Id"] != env.EventId {
			t.Fatal("archival event identity header differs")
		}
		if parked {
			if headers[bus.HeaderDLQOriginalSubject] != env.EventType || headers[bus.HeaderDLQClass] != bus.ClassTerminal {
				t.Fatal("cross-tenant terminal parking metadata differs")
			}
		} else if !bytes.Equal(msg.Key, []byte(env.PartitionKey)) {
			t.Fatal("archival partition key differs")
		}
	}
	for i, state := range []lakepb.SnapshotState{1, 2, 3, 4} {
		read(reader, publish(state, tenant), int64(i), false)
	}
	// Ensure prior ACKs actually reached the native durable before graceful
	// stop, so restart must continue rather than replay a merely read record.
	waitAck := func(min uint64) {
		t.Helper()
		for {
			c, e := js.Consumer(ctx, snapshotprofile.Name, "archiver-lake_snapshot__")
			if e == nil {
				info, e := c.Info(ctx)
				if e == nil && info.AckFloor.Consumer >= min && info.NumAckPending == 0 {
					return
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("archiver acknowledgement unavailable")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitAck(4)
	archiver.close(t)
	wire := publish(3, tenant)
	archiver = startActualSnapshotArchiver(t, ctx, b, broker, tenant)
	read(reader, wire, 4, false)
	foreign := publish(3, "foreign-tenant")
	read(dlq, foreign, 0, true)
	// Positive next marker proves reader progress and that the foreign frame was
	// not archived into the tenant topic; an absence timeout cannot pass.
	read(reader, publish(3, tenant), 5, false)
	waitAck(7)
	archiver.close(t)
}
