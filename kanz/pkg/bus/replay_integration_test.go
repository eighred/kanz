package bus_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
)

// A LOG FOLD AGAINST A REAL BROKER, AND THE NEGATIVE CONTROL THAT MAKES IT MEAN
// SOMETHING (#112).
//
// SubscribeReplay and SubscribeBroadcastReady differ by ONE enum, and the unit
// tests cannot see the difference — both fakes just hand over whatever they hold.
// Only a real JetStream server distinguishes DeliverAll from
// DeliverLastPerSubject, so this test runs BOTH against the same three published
// messages on the same subject and asserts they disagree:
//
//	replay    -> all three, oldest first
//	broadcast -> the last one only
//
// If SubscribeReplay ever silently became a broadcast, the model registry would
// fold a promotion without the validation it depends on, the MLOPS-01a gate would
// refuse it, and the replica would report "no model serves this contract" on a
// fleet that has one. That is exactly the regression this catches, and nothing
// else in the suite can.
func TestNATSSubscribeReplayDeliversTheWholeLog(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_REPLAY_" + suffix
	subject := "test.replay." + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	defer setupConn.Close()
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	defer func() { _ = setupJS.DeleteStream(context.Background(), streamName) }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "replay-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Three entries on ONE subject, which is what an append log looks like.
	want := []string{"register", "validate", "promote"}
	for _, body := range want {
		if err := client.Publish(ctx, bus.Message{Subject: subject, Body: []byte(body)}); err != nil {
			t.Fatalf("publish %s: %v", body, err)
		}
	}

	replayed := collect(t, ctx, len(want), func(c context.Context, h bus.Handler, ready func()) error {
		return client.SubscribeReplay(c, subject, h, ready)
	})
	if len(replayed) != len(want) {
		t.Fatalf("replay delivered %v, want all of %v — a fold that starts anywhere but the "+
			"beginning is not a rebuild, it is a guess", replayed, want)
	}
	for i := range want {
		if replayed[i] != want[i] {
			t.Fatalf("replay order = %v, want %v — a promotion applied before the validation it "+
				"depends on is refused by the MLOPS-01a gate, on some replicas and not others",
				replayed, want)
		}
	}

	// THE NEGATIVE CONTROL. Same subject, same messages, the other delivery
	// policy: exactly one message. If this ever starts returning three, the two
	// subscriptions have collapsed into one and the test above proves nothing.
	broadcast := collect(t, ctx, 1, func(c context.Context, h bus.Handler, ready func()) error {
		return client.SubscribeBroadcastReady(c, subject, h, ready)
	})
	if len(broadcast) != 1 || broadcast[0] != "promote" {
		t.Fatalf("broadcast delivered %v, want just [promote] — DeliverLastPerSubject and "+
			"DeliverAll are indistinguishable in this suite if they agree here", broadcast)
	}
}

// collect runs one subscription until it has armed AND seen at least min
// messages, then cancels and returns the bodies in delivery order.
func collect(t *testing.T, parent context.Context, minMsgs int,
	subscribe func(context.Context, bus.Handler, func()) error) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var mu sync.Mutex
	var got []string
	enough := make(chan struct{})
	var once sync.Once

	go func() {
		_ = subscribe(ctx, func(_ context.Context, m bus.Message) error {
			mu.Lock()
			got = append(got, string(m.Body))
			n := len(got)
			mu.Unlock()
			if n >= minMsgs {
				once.Do(func() { close(enough) })
			}
			return nil
		}, nil)
	}()

	select {
	case <-enough:
	case <-time.After(15 * time.Second):
		t.Fatal("subscription never delivered the messages it was supposed to")
	}
	// A short settle so an EXTRA delivery (the failure the negative control is
	// looking for) has a chance to arrive rather than being cut off by the cancel.
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...)
}
