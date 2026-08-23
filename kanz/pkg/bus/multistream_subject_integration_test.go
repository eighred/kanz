// A SUBJECT THAT MATCHES SEVERAL STREAMS IS REFUSED, NOT SILENTLY NARROWED
// (#698), against a REAL spine.
//
// No fake can show this one. The defect lives entirely inside the JetStream
// client: StreamNameBySubject ends `return resp.Streams[0], nil`, so a subject
// matching sixteen streams resolved to one — the first in the SERVER's order —
// with no error, no warning, and a consumer that then worked perfectly on the
// stream it happened to pick. A stub subscriber has no streams at all and would
// be green through every version of this code.
//
// It cost the compliance record: services/audit ran on DefaultSubjects = [">"]
// with the comment "materializes everything", bound one stream, and materialized
// nothing from the other fifteen — Ready throughout, consume counter climbing.
//
//	nats-server -js -p 4222          # no Docker needed
//	TEST_NATS_URL=nats://localhost:4222 go test ./pkg/bus/...
package bus_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
)

func TestIntegration_SubscribeRefusesASubjectMatchingSeveralStreams(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the multi-stream refusal over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	// Two streams under ONE prefix — the shape RISK and POSITION already have on
	// this estate (risk.portfolio.> and risk.position.>, split because POSITION is
	// compacted). A subject spanning them is the thing under test.
	const (
		streamA = "KANZTEST_MULTI_A"
		streamB = "KANZTEST_MULTI_B"
	)
	for name, subj := range map[string]string{
		streamA: "kanztest.multi.alpha.>",
		streamB: "kanztest.multi.beta.>",
	} {
		if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name: name, Subjects: []string{subj}, MaxAge: time.Minute,
		}); err != nil {
			t.Fatalf("create stream %s: %v", name, err)
		}
		t.Cleanup(func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			_ = js.DeleteStream(dctx, name)
		})
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "multistream-probe"})
	if err != nil {
		t.Fatalf("bus client: %v", err)
	}
	defer client.Close()

	// `kanztest.multi.>` spans both. Before this change the client would have
	// picked whichever the server listed first and consumed only that one.
	err = client.Subscribe(ctx, "kanztest.multi.>", "kanztest-multi-group",
		func(context.Context, bus.Message) error { return nil })

	if err == nil {
		t.Fatal("Subscribe ACCEPTED a subject matching two streams. It binds ONE stream, so one of " +
			"them is now silently unconsumed — the shape that left fifteen of sixteen streams out " +
			"of the audit log (#698)")
	}
	// The message has to name what it found, or an operator debugging a missing
	// event learns only that "something" was wrong with a subject that looks fine.
	for _, want := range []string{"matches 2 streams", streamA, streamB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q — an operator cannot act on it:\n%v", want, err)
		}
	}
}

// The other half of the contract: a subject resolving to exactly one stream must
// still subscribe. Without this, "refuse multi-stream subjects" could be
// implemented as "refuse everything" and the test above would still pass.
func TestIntegration_SubscribeStillBindsASubjectMatchingOneStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the single-stream binding over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const stream = "KANZTEST_SINGLE"
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: stream, Subjects: []string{"kanztest.single.>"}, MaxAge: time.Minute,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = js.DeleteStream(dctx, stream)
	})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "multistream-probe"})
	if err != nil {
		t.Fatalf("bus client: %v", err)
	}
	defer client.Close()

	// Subscribe blocks until ctx is done, so run it and give it a moment to
	// either bind or fail. A binding failure returns promptly.
	subCtx, subCancel := context.WithTimeout(ctx, 3*time.Second)
	defer subCancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Subscribe(subCtx, "kanztest.single.>", "kanztest-single-group",
			func(context.Context, bus.Message) error { return nil })
	}()
	select {
	case err := <-errCh:
		// context deadline is the healthy outcome: it bound, then ran until the
		// deadline. Anything else is a binding failure.
		if err != nil && !strings.Contains(err.Error(), "context deadline exceeded") &&
			!strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Subscribe refused a subject matching exactly one stream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Subscribe neither bound nor returned within 10s")
	}
}
