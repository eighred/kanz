//go:build linux && amd64

package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func TestSnapshotActualRequestedParkingPreservesOriginalOrRetainsFailure(t *testing.T) {
	for _, mode := range []string{"terminal-poison", "parking-refused", "transient-retained"} {
		t.Run(mode, func(t *testing.T) {
			b := newSnapshotBroker(t)
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			admin, err := b.connect(b.adminA)
			if err != nil {
				t.Fatal("fixture administrator unavailable")
			}
			defer admin.Close()
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatal("fixture JetStream unavailable")
			}
			stream, err := js.CreateStream(ctx, snapshotprofile.Stream())
			if err != nil {
				t.Fatal("retained stream unavailable")
			}
			provisionSnapshotRegisteredDependencies(t, ctx, js)
			tuning := bus.RetainedWorkTuning()
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: "nats://" + b.address, TLSConfig: transport.ClientTLSConfig(b.source, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/nats"))), DisableReconnect: true, ConsumerTuning: &tuning, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal("scoped workload unavailable")
			}
			defer func() { _ = client.Close() }()
			if client.RequireCanonicalParking(ctx, SubjectRequested, int64(snapshotprofile.Stream().MaxMsgSize)) != nil {
				t.Fatal("positive parking preflight failed")
			}
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			failed := make(chan struct{})
			var failOnce sync.Once
			subscriber := &latchedSubscriber{NATSClient: client, fail: func() { failOnce.Do(func() { close(failed); cancel() }) }}
			consumer, err := bus.NewConsumer(subscriber, bus.WithRetainedRetries(SubjectRequested), bus.WithDLQ(client))
			if err != nil {
				t.Fatal("retained consumer unavailable")
			}
			ready := make(chan struct{})
			done := make(chan error, 1)
			var readyOnce sync.Once
			var calls atomic.Int32
			go func() {
				done <- consumer.SubscribeReady(work, SubjectRequested, snapshotprofile.Group, func(context.Context, *envelopepb.Envelope, []byte) error {
					calls.Add(1)
					return errors.New("fixture dependency unavailable")
				}, func() { readyOnce.Do(func() { close(ready) }) })
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("consumer cleanup unconfirmed")
					}
				}
			}()
			select {
			case <-ready:
			case <-done:
				joined = true
				t.Fatal("consumer stopped before registration")
			case <-ctx.Done():
				t.Fatal("consumer registration deadline")
			}
			parking, err := js.Stream(ctx, "DLQ")
			if err != nil {
				t.Fatal("parking stream unavailable")
			}
			if mode == "parking-refused" {
				info, e := parking.Info(ctx)
				if e != nil {
					t.Fatal("parking profile unavailable")
				}
				changed := info.Config
				changed.MaxMsgSize = 1
				if _, e = js.UpdateStream(ctx, changed); e != nil {
					t.Fatal("parking refusal control unavailable")
				}
			}
			original := &nats.Msg{Subject: SubjectRequested, Data: []byte{0xff, 0xff, 0xff}, Header: nats.Header{"X-Fixture-Custody": []string{"original"}}}
			if mode == "transient-retained" {
				// The capture only obtains bytes from the canonical Producer; actual
				// transport acceptance/dispatch and retention are independently below.
				capture := &lifecycleCapture{}
				producer, e := bus.NewProducer(capture, bus.ProducerConfig{Source: Service, ProducerVersion: "fixture", Tenant: "tenant-a"})
				if e != nil {
					t.Fatal("canonical producer unavailable")
				}
				record, e := lifecycleRecord(ctx, "tenant-a", &lakepb.Snapshot{SnapshotId: "snapshot-parking-fixture", State: lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}, bytes.Repeat([]byte{1}, 32), 0, time.Now())
				if e != nil {
					t.Fatal("canonical lifecycle unavailable")
				}
				event, e := record.Event()
				if e != nil || producer.Publish(ctx, event) != nil || len(capture.messages) != 1 {
					t.Fatal("canonical lifecycle framing failed")
				}
				original.Data = capture.messages[0].Body
				for key, value := range capture.messages[0].Headers {
					original.Header.Set(key, value)
				}
			}
			ack, err := js.PublishMsg(ctx, original)
			if err != nil {
				t.Fatal("original Requested publication failed")
			}
			if mode != "terminal-poison" {
				select {
				case <-failed:
				case <-ctx.Done():
					t.Fatal("failure did not latch owner cancellation")
				}
				select {
				case <-done:
					joined = true
				case <-ctx.Done():
					t.Fatal("failed consumer did not join")
				}
				retained, e := stream.GetMsg(ctx, ack.Sequence)
				if e != nil || !bytes.Equal(retained.Data, original.Data) {
					t.Fatal("failed delivery lost original bytes")
				}
				for key, values := range original.Header {
					if !equalStrings(retained.Header.Values(key), values) {
						t.Fatal("failed delivery changed original headers")
					}
				}
				c, e := js.Consumer(ctx, snapshotprofile.Name, snapshotprofile.Durable)
				if e != nil {
					t.Fatal("retained consumer unavailable")
				}
				info, e := c.Info(ctx)
				if e != nil || info.Delivered.Consumer < 1 || info.AckFloor.Consumer != 0 {
					t.Fatal("failed delivery was acknowledged")
				}
				parked, e := parking.Info(ctx)
				if e != nil || parked.State.Msgs != 0 {
					t.Fatal("failed or transient delivery incorrectly parked")
				}
				if (mode == "transient-retained") != (calls.Load() > 0) {
					t.Fatal("dispatch classification differs")
				}
				return
			}
			for {
				message, e := parking.GetLastMsgForSubject(ctx, "dlq."+SubjectRequested)
				if e == nil {
					if !bytes.Equal(message.Data, original.Data) || message.Header.Get(bus.HeaderDLQOriginalSubject) != SubjectRequested || message.Header.Get(bus.HeaderDLQClass) != bus.ClassTerminal || message.Header.Get(bus.HeaderDLQAttempts) != "0" || calls.Load() != 0 {
						t.Fatal("canonical poison parking changed original or dispatch class")
					}
					for key, values := range original.Header {
						if !equalStrings(message.Header.Values(key), values) {
							t.Fatal("parking dropped original header")
						}
					}
					c, e := js.Consumer(ctx, snapshotprofile.Name, snapshotprofile.Durable)
					if e == nil {
						info, e := c.Info(ctx)
						if e == nil && info.AckFloor.Consumer == 1 && info.NumAckPending == 0 {
							break
						}
					}
				}
				select {
				case <-failed:
					t.Fatal("successful parking latched failure")
				case <-ctx.Done():
					t.Fatal("poison parking/ACK unavailable")
				case <-time.After(20 * time.Millisecond):
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
