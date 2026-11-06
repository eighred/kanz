package bus_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestNATSDurableReadyWaitsForPendingAndAckPending(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "TV_READY_" + suffix
	subject := "test.ready." + suffix
	group := "ready-" + suffix
	st, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{subject}, Storage: jetstream.MemoryStorage})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = js.DeleteStream(cleanup, name)
	}()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "ready-fixture", ConsumerTuning: &bus.ConsumerTuning{AckWait: 10 * time.Second, MaxDeliver: 5, MaxAckPending: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	publishProbes(t, ctx, client, subject, 2)
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	first, second := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	defer firstOnce.Do(func() { close(first) })
	defer secondOnce.Do(func() { close(second) })
	entered := make(chan int, 2)
	armed := make(chan struct{}, 2)
	done := make(chan error, 1)
	var handled atomic.Int32
	subctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		done <- consumer.SubscribeReady(subctx, subject, group, func(hctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
			n := int(handled.Add(1))
			entered <- n
			block := first
			if n == 2 {
				block = second
			}
			select {
			case <-block:
				return nil
			case <-hctx.Done():
				return hctx.Err()
			}
		}, func() { armed <- struct{}{} })
	}()
	waitEntered := func(want int) {
		t.Helper()
		select {
		case n := <-entered:
			if n != want {
				t.Fatalf("delivery%d want%d", n, want)
			}
		case <-ctx.Done():
			t.Fatal("delivery timed out")
		}
	}
	info := func() *jetstream.ConsumerInfo {
		t.Helper()
		for value := range st.ListConsumers(ctx).Info() {
			if value.Config.FilterSubject == subject && value.Config.Durable != "" {
				return value
			}
		}
		t.Fatal("durable missing")
		return nil
	}
	notArmed := func() {
		t.Helper()
		select {
		case <-armed:
			t.Fatal("unacknowledged backlog armed")
		case <-time.After(150 * time.Millisecond):
		}
	}
	waitEntered(1)
	before := info()
	if before.NumPending != 1 || before.NumAckPending != 1 {
		t.Fatalf("initial pending=%d ack=%d", before.NumPending, before.NumAckPending)
	}
	notArmed()
	firstOnce.Do(func() { close(first) })
	waitEntered(2)
	during := info()
	if during.NumPending != 0 || during.NumAckPending != 1 {
		t.Fatalf("inflight pending=%d ack=%d", during.NumPending, during.NumAckPending)
	}
	notArmed()
	secondOnce.Do(func() { close(second) })
	select {
	case <-armed:
	case <-ctx.Done():
		t.Fatal("drained durable did not arm")
	}
	complete := info()
	if complete.NumPending != 0 || complete.NumAckPending != 0 || handled.Load() != 2 {
		t.Fatal("armed before complete fold")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("durable did not drain")
	}
	// Fresh Consumer has no dedup memory. Same durable must resume its ack cursor,
	// not become an ephemeral replay or replay old messages hidden by dedup.
	restarted, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	restartCtx, stopRestart := context.WithCancel(ctx)
	defer stopRestart()
	go func() {
		done <- restarted.SubscribeReady(restartCtx, subject, group, func(context.Context, *envelopepb.Envelope, []byte) error { handled.Add(1); return nil }, func() { armed <- struct{}{} })
	}()
	select {
	case <-armed:
	case <-ctx.Done():
		t.Fatal("empty resumed durable did not arm")
	}
	after := info()
	if after.Name != before.Name || !after.Created.Equal(before.Created) || after.AckFloor.Stream != complete.AckFloor.Stream || after.Config.DeliverPolicy != before.Config.DeliverPolicy || handled.Load() != 2 {
		t.Fatal("durable identity/cursor/delivery contract changed")
	}
	stopRestart()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("restarted durable did not drain")
	}
}
