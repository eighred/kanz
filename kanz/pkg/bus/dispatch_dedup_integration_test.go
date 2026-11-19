package bus_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The broker accepts each transport copy. Consumer-side dedup must let both
// independent durable groups observe the same canonical envelope exactly once.
// Business payload validation and OMS/TCA/position effects have separate tests.
func TestNATSConsumerDedupKeepsIndependentDurables(t *testing.T) {
	srv, err := ns.NewServer(&ns.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
		NoSigs: true, NoLog: true, MaxPayload: 1 << 20, MaxConn: 8,
	})
	if err != nil {
		t.Fatal("disposable broker configuration refused")
	}
	srv.Start()
	t.Cleanup(func() {
		srv.Shutdown()
		done := make(chan struct{})
		go func() { srv.WaitForShutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("disposable broker shutdown was not joined")
		}
	})
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("disposable broker did not become ready")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	url := "nats://" + srv.Addr().String()
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second), nats.NoReconnect())
	if err != nil {
		t.Fatal("disposable broker connection refused")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	const subject = "market.equity.trade"
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "DISPATCH", Subjects: []string{subject}, Storage: jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy, MaxMsgs: 64, MaxBytes: 2 << 20, MaxAge: time.Minute,
	})
	if err != nil {
		t.Fatal("disposable stream provisioning refused")
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "dispatch-fixture", DrainGrace: time.Second})
	if err != nil {
		t.Fatal("typed bus connection refused")
	}
	t.Cleanup(func() { _ = client.Close() })
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	subctx, stop := context.WithCancel(ctx)
	armed, received := make(chan struct{}, 2), make(chan int, 8)
	done := make(chan error, 2)
	defer func() {
		stop()
		for range 2 {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("subscription returned an error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("subscription shutdown was not joined")
			}
		}
	}()
	var calls [2]atomic.Int32
	for i, group := range []string{"position-projection", "tca"} {
		go func() {
			done <- consumer.SubscribeReady(subctx, subject, group, func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
				if env.IdempotencyKey != "same-fact" || env.TenantId != "acme" {
					return bus.Terminal(fmt.Errorf("canonical envelope binding changed"))
				}
				calls[i].Add(1)
				received <- i
				return nil
			}, func() { armed <- struct{}{} })
		}()
	}
	for range 2 {
		select {
		case <-armed:
		case <-ctx.Done():
			t.Fatal("independent durables did not arm")
		}
	}
	env := validEnvelope()
	env.EventId, env.IdempotencyKey = "same-fact", "same-fact"
	body := frame(t, env, nil)
	for i := range 3 {
		if err := client.Publish(ctx, bus.Message{Subject: subject, Body: body,
			Headers: map[string]string{"Nats-Msg-Id": fmt.Sprintf("transport-copy-%d", i)}}); err != nil {
			t.Fatal("transport copy was not accepted")
		}
	}
	seen := map[int]bool{}
	for len(seen) != 2 {
		select {
		case i := <-received:
			seen[i] = true
		case <-ctx.Done():
			t.Fatal("a required durable never dispatched its canonical fact")
		}
	}
	// Read the actual broker ACK floor; an idle callback or a short sleep would
	// not prove that duplicate transport deliveries passed through both groups.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		complete := 0
		for info := range stream.ListConsumers(ctx).Info() {
			if info.Config.FilterSubject == subject && info.AckFloor.Stream >= 3 && info.NumAckPending == 0 && info.NumPending == 0 {
				complete++
			}
		}
		if complete == 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("both durables did not acknowledge all three transport copies")
		}
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 3 {
		t.Fatal("broker-side suppression concealed the consumer duplicate test")
	}
	for i := range calls {
		if calls[i].Load() != 1 {
			t.Fatalf("durable %d dispatched %d times, want 1", i, calls[i].Load())
		}
	}
}
