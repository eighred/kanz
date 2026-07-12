// The venue adapter's publish-failure path against a REAL spine (EXEC-M8).
//
// This is the capital control on this platform, and until now it was proven only
// against a fake publisher that returned an error because a test told it to. That
// is the same class of evidence that let market-ingest ship: every unit test on
// its publish path used a fake Publisher, so nobody discovered that a real broker
// rejected every envelope it produced while /readyz answered 200.
//
// A fake publisher cannot fail the way a broker fails. So this drives the REAL
// bus.Producer over REAL NATS/JetStream, through the REAL bus.HealthPublisher the
// adapters wire, into the REAL Probes() handler the kubelet hits — and then breaks
// the broker underneath it in the two ways production actually breaks:
//
//   - the stream the adapter publishes to is gone (a misconfigured or deleted
//     stream — the EXEC-M7a bug: bus.Publish is a JetStream publish, so an unbound
//     subject is a hard failure, not fire-and-forget), and
//   - the broker itself is unreachable (the connection is closed under it).
//
// What must happen: /readyz turns 503, the kubelet pulls the pod out of its
// Service, and the OMS can no longer route orders into an adapter that would
// execute them at a live exchange and lose the fills. Refusing to trade beats
// trading blind.
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test ./internal/venueadapter/server/...
package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kanz-eng/kanz/internal/venueadapter/server"
	"github.com/kanz-eng/kanz/pkg/bus"
)

const itStream = "EXECUTION_VENUE_IT"

// fillEvent is the FACT an adapter publishes after it works an order at the
// exchange. If these stop landing, the adapter is trading real money and losing
// the record of it.
//
// The subject is scoped to this test (not the production "order.fill.recorded")
// for one reason: `go test ./...` runs packages in PARALLEL against ONE broker, and
// the translate integration test binds a stream to `order.>`. Two streams cannot
// claim overlapping subjects, so a realistic-looking subject here would make the
// two tests fight over the broker and fail each other. Nothing under test cares
// what the subject is called — this exercises the real Producer, the real
// HealthPublisher and the real probe handler against a real JetStream, and what it
// breaks is the BINDING, which is subject-agnostic.
func fillEvent() bus.Event {
	return bus.Event{
		Subject:       "venueit.order.fill.recorded",
		EventType:     "order.v1.Fill",
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "order",
		EventTime:     time.Now().UTC(),
		TenantID:      "acme-capital",
		PartitionKey:  "BTC-USD",
		Payload: &orderpb.Fill{
			FillId:  "fill-1",
			OrderId: "order-1",
		},
	}
}

// probe hits the real kubelet-facing handler.
func probe(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, rec.Body.String()
}

func TestIntegration_PublishFailureEvictsTheAdapter(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the venue adapter's publish-health path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The stream the adapter's FACTs land on (mirrors infra/nats/bootstrap-job.yaml).
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	// t.Cleanup, not defer: a deferred Close runs BEFORE the cleanups, which would
	// tear the connection down under the DeleteStream below and leak the stream
	// (with its messages) into the next run.
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("admin jetstream: %v", err)
	}
	createStream := func() {
		t.Helper()
		// Idempotent: a previous run that died mid-test must not poison this one.
		_ = js.DeleteStream(ctx, itStream)
		if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:      itStream,
			Subjects:  []string{"venueit.>"},
			Storage:   jetstream.MemoryStorage,
			Retention: jetstream.LimitsPolicy,
		}); err != nil {
			t.Fatalf("create stream: %v", err)
		}
	}
	createStream()
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), itStream) })

	// The adapter, wired exactly as venue-binance/venue-okx wire it.
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "venue-it", MaxReconnects: 1})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", ProducerVersion: "exec-m8-it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	health := bus.NewHealthPublisher(producer, 3)

	readiness := &server.Readiness{}
	readiness.Set(true) // the process is up and the exchange handshake succeeded
	readiness.TrackPublisher(health)
	probes := server.Probes(readiness, nil)

	// 1. A working spine. The fill actually lands — this is not "no error", it is
	//    the event coming back off the wire.
	if err := health.Publish(ctx, fillEvent()); err != nil {
		t.Fatalf("publish to a healthy spine failed: %v", err)
	}
	info, err := js.Stream(ctx, itStream)
	if err != nil {
		t.Fatal(err)
	}
	st, err := info.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State.Msgs != 1 {
		t.Fatalf("the fill did not reach the stream: %d msgs", st.State.Msgs)
	}
	if code, _ := probe(t, probes); code != http.StatusOK {
		t.Fatalf("/readyz = %d on a healthy spine, want 200 — the adapter would never take an order", code)
	}

	// 2. THE STREAM IS GONE. bus.Publish is a JetStream publish, so the subject is
	//    now unbound and every FACT this adapter produces has nowhere to land. This
	//    is EXEC-M7a's failure, reproduced against a real broker.
	if err := js.DeleteStream(ctx, itStream); err != nil {
		t.Fatalf("delete stream: %v", err)
	}
	var lastErr error
	for i := 0; i < 3; i++ { // the threshold: 3 CONSECUTIVE failures
		lastErr = health.Publish(ctx, fillEvent())
		if lastErr == nil {
			t.Fatalf("publish %d succeeded with no stream bound to the subject", i+1)
		}
	}
	code, body := probe(t, probes)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d after the spine stopped accepting fills, want 503.\n"+
			"THIS IS THE CAPITAL CONTROL: the adapter would keep answering gRPC and executing orders at a live exchange "+
			"while every fill it produced vanished, and the OMS would never learn what it had bought. Last publish error: %v",
			code, lastErr)
	}
	t.Logf("/readyz = 503 %q", body)

	// 3. RECOVERY. A transient outage must not permanently condemn an adapter that
	//    is otherwise fine — a pod that never comes back is its own outage.
	createStream()
	if err := health.Publish(ctx, fillEvent()); err != nil {
		t.Fatalf("publish after the stream came back: %v", err)
	}
	if code, _ := probe(t, probes); code != http.StatusOK {
		t.Fatalf("/readyz = %d after recovery, want 200 — the adapter never rejoins its Service", code)
	}

	// 4. THE BROKER ITSELF IS GONE. Not a missing stream — no NATS at all.
	_ = client.Close()
	for i := 0; i < 3; i++ {
		if err := health.Publish(ctx, fillEvent()); err == nil {
			t.Fatal("a publish SUCCEEDED against a closed broker connection — the fill went nowhere and nobody was told")
		}
	}
	if code, _ := probe(t, probes); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with the broker gone, want 503", code)
	}
}
