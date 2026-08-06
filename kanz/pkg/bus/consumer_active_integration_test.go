package bus_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
)

// TestConsumerActiveSeesAConsumerComeAndGo drives NATSClient.ConsumerActive
// against a REAL broker, because the thing it reports on — whether a durable is
// being worked — has no meaning against a fake.
//
// WHAT DEPENDS ON THIS. archiver-drain re-produces parked events to the same
// Kafka topics services/archiver writes, and the archiver is a single writer by
// deployment: two producers can invert per-key order, and a reordered log
// rebuilds a different book downstream. The drain refuses to run unless this
// returns false. A ConsumerActive that answered false while the archiver was
// consuming would not fail loudly — it would let a second writer start, and the
// damage would surface later as a book that does not reconcile.
//
// The two directions are NOT equally dangerous, so they are asserted separately:
//
//	true while consuming   → the SAFETY property. A false here starts a second writer.
//	false after it stops   → the USABILITY property. A true here only blocks a drain.
func TestConsumerActiveSeesAConsumerComeAndGo(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set — ConsumerActive is meaningless without a real broker")
	}
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	streamName := "TEST_ACTIVE_" + suffix
	subject := "test.active." + suffix
	group := "active-" + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name: streamName, Subjects: []string{subject},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = setupJS.DeleteStream(ctx, streamName)
		setupConn.Close()
	})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "active-probe"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// BEFORE ANY SUBSCRIBE the durable does not exist, and that must be an ERROR
	// rather than a calm false. "There is no such consumer" and "the consumer is
	// idle" are different facts, and a caller gating a destructive action on a
	// bare false would treat a typo'd group name as permission to proceed.
	if _, err := client.ConsumerActive(ctx, subject, group); err == nil {
		t.Fatal("ConsumerActive returned no error for a durable that does not exist. A caller gating a " +
			"second writer on this would read a mistyped group as 'nobody is consuming' and start anyway")
	}

	subCtx, stopSub := context.WithCancel(ctx)
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		_ = client.Subscribe(subCtx, subject, group, func(context.Context, bus.Message) error { return nil })
	}()

	// THE SAFETY DIRECTION.
	if !waitForActive(t, client, subject, group, true, 30*time.Second) {
		stopSub()
		<-subDone
		t.Fatal("ConsumerActive stayed FALSE while a Subscribe was running. This is the direction that " +
			"matters: archiver-drain would take it as permission to produce beside the live archiver, " +
			"and two producers can invert per-key order in Kafka")
	}

	stopSub()
	<-subDone

	// THE USABILITY DIRECTION. Pull requests EXPIRE rather than vanishing, so this
	// legitimately lags the shutdown — the budget is generous for that reason, and
	// a failure here means the drain could never be run at all, not that anything
	// is unsafe.
	if !waitForActive(t, client, subject, group, false, 90*time.Second) {
		t.Error("ConsumerActive never returned to FALSE after the subscriber stopped, so a drain gated " +
			"on it could never run. Outstanding pull requests should expire; if this is consistently " +
			"slow the gate needs a different signal, not a longer wait")
	}
}

// waitForActive polls until ConsumerActive reports want, or the budget runs out.
// A probe ERROR is not treated as either answer — it is retried, because the
// broker not answering is not evidence about the consumer.
func waitForActive(t *testing.T, c *bus.NATSClient, subject, group string, want bool, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		got, err := c.ConsumerActive(context.Background(), subject, group)
		if err == nil && got == want {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
