package snapshot

import (
	"bytes"
	"context"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

type lifecycleCapture struct{ messages []bus.Message }

func (c *lifecycleCapture) Publish(_ context.Context, m bus.Message) error {
	c.messages = append(c.messages, m)
	return nil
}
func (*lifecycleCapture) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (*lifecycleCapture) Close() error                                                 { return nil }

// This exercises canonical stamping/validation, not broker durability.
func TestLifecycleRecordsPassCanonicalProducerValidation(t *testing.T) {
	ctx := context.Background()
	client := &lifecycleCapture{}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: Service, ProducerVersion: "fixture", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	states := []lakepb.SnapshotState{lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED, lakepb.SnapshotState_SNAPSHOT_STATE_CAPTURING, lakepb.SnapshotState_SNAPSHOT_STATE_READY, lakepb.SnapshotState_SNAPSHOT_STATE_FAILED}
	subjects := []string{SubjectRequested, "lake.snapshot.capturing", "lake.snapshot.published", "lake.snapshot.failed"}
	for i, state := range states {
		snapshot := &lakepb.Snapshot{SnapshotId: "snapshot-fixture-0001", State: state, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}
		record, err := lifecycleRecord(ctx, "tenant-a", snapshot, bytes.Repeat([]byte{1}, 32), int64(i), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		event, err := record.Event()
		if err != nil {
			t.Fatal(err)
		}
		if err = producer.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
		msg := client.messages[i]
		env, payload, err := bus.Unframe(msg.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err = bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var fact lakepb.SnapshotLifecycle
		if err = proto.Unmarshal(payload, &fact); err != nil {
			t.Fatal(err)
		}
		if msg.Subject != subjects[i] || env.EventType != subjects[i] || env.TenantId != "tenant-a" || env.Source != Service || fact.SnapshotId != snapshot.SnapshotId || fact.State != state || fact.SourceScope != snapshot.SourceScope || !bytes.Equal(fact.RequestSha256, bytes.Repeat([]byte{1}, 32)) {
			t.Fatal("canonical lifecycle attribution changed")
		}
	}
}
