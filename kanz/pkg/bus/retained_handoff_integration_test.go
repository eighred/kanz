package bus

import (
	"context"
	"fmt"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"os"
	"testing"
	"time"
)

func TestRetainedProfilePreservesExistingDurableCursor(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	subject := "test.retained.handoff." + suffix
	name := "RETAINED_HANDOFF_" + suffix
	group := "handoff-" + suffix
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{subject}, Storage: jetstream.MemoryStorage})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = js.DeleteStream(cleanup, name)
	}()
	old, err := DialNATS(ctx, NATSConfig{URL: url, Name: "old-handoff"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := old.Publish(ctx, Message{Subject: subject, Body: []byte("already-acknowledged")}); err != nil {
		t.Fatal(err)
	}
	oldctx, stopOld := context.WithCancel(ctx)
	defer stopOld()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- old.Subscribe(oldctx, subject, group, func(context.Context, Message) error { return nil })
	}()
	var before *jetstream.ConsumerInfo
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for before == nil {
		if cons, err := js.Consumer(ctx, name, durableName(group, subject)); err == nil {
			if info, err := cons.Info(ctx); err == nil && info.AckFloor.Stream == 1 {
				before = info
			}
		}
		if before == nil {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("old durable did not acknowledge seed")
			}
		}
	}
	if before.Config.MaxDeliver != 64 {
		t.Fatal("baseline work profile changed")
	}
	stopOld()
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("old durable did not stop")
	}
	if err := old.Publish(ctx, Message{Subject: subject, Body: []byte("pending-after-handoff")}); err != nil {
		t.Fatal(err)
	}
	tuning := RetainedWorkTuning()
	next, err := DialNATS(ctx, NATSConfig{URL: url, Name: "retained-handoff", ConsumerTuning: &tuning})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	nextctx, stopNext := context.WithCancel(ctx)
	defer stopNext()
	done := make(chan error, 1)
	received := make(chan string, 4)
	ready := make(chan struct{}, 1)
	go func() {
		done <- next.SubscribeReady(nextctx, subject, group, func(_ context.Context, m Message) error { received <- string(m.Body); return nil }, func() { ready <- struct{}{} })
	}()
	defer func() {
		stopNext()
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("new durable failed to drain")
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("same durable did not catch up after supported tuning update")
	}
	select {
	case body := <-received:
		if body != "pending-after-handoff" {
			t.Fatal("acknowledged seed replayed after profile update")
		}
	default:
		t.Fatal("pending message lost")
	}
	select {
	case <-received:
		t.Fatal("handoff reset cursor or duplicated delivery")
	default:
	}
	cons, err := js.Consumer(ctx, name, before.Name)
	if err != nil {
		t.Fatal(err)
	}
	after, err := cons.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Config.MaxDeliver != -1 || after.Name != before.Name || !after.Created.Equal(before.Created) || after.AckFloor.Stream != 2 {
		t.Fatalf("same durable/cursor not preserved: before=%+v after=%+v", before, after)
	}
}
